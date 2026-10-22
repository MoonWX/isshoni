import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { StrictMode } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { createMemoryRouter, Link, matchRoutes, Outlet, RouterProvider, type RouteObject } from 'react-router';

import { clearLog, logLines } from '../lib/log';
import type { AuthNotice, AuthState } from '../platform/desktop/bridge';
import { BridgeError } from '../platform/desktop/errors';
import { authState } from '../platform/desktop/testing/fakeBridge';
import { queryKeys } from '../protocol/queryKeys';
import { api } from '../protocol/rest';
import { apiError, apiPath, meFixture, server, signedIn } from '../test/msw';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderRoute } from '../test/render';
import { AppProviders } from './App';
import {
  desktopSignedOutPath,
  RequireAdmin,
  RequireAuth,
  RequireBrowser,
  RequireDesktop,
  RequireInviter,
  safeNext,
  loginPath,
} from './guards';
import { NotFound } from './NotFound';
import {
  createAppRoutes,
  defaultSources,
  preloadRoute,
  routeTargetOf,
  type AppRouteSources,
  type FolderModule,
  type RouteHandle,
  type RouteTarget,
} from './router';
import { createDesktopTestPlatform } from './testing/desktop';

const none: AppRouteSources = { lazy: {} };

type Access = 'public' | 'user' | 'admin' | 'inviter';
/** Which platform shows a page (M2, docs/m2/03 §7.5): RequireBrowser's pages, RequireDesktop's, or both. */
type Where = 'both' | 'browser' | 'desktop';

/** 05 §5 and docs/m2/03 §7.5, row by row: path → [folder, page, access, where]. */
const TABLE: [string, RouteHandle['folder'] | null, string, Access, Where][] = [
  ['/', 'rooms', 'RootRedirect', 'user', 'both'],
  ['/r/lounge', 'rooms', 'RoomPage', 'user', 'both'],
  ['/login', 'auth', 'LoginPage', 'public', 'browser'],
  ['/invite', 'auth', 'InvitePage', 'public', 'browser'],
  ['/signup', 'auth', 'SignupPage', 'public', 'browser'],
  ['/pending', 'auth', 'PendingPage', 'public', 'browser'],
  ['/reset', 'auth', 'ResetPage', 'public', 'browser'],
  ['/setup', 'setup', 'SetupPage', 'public', 'browser'],
  ['/admin/welcome', 'setup', 'WelcomePage', 'admin', 'browser'],
  ['/download', 'download', 'DownloadPage', 'public', 'browser'],
  ['/about', 'auth', 'AboutPage', 'public', 'both'],
  ['/account', 'account', 'AccountPage', 'user', 'browser'],
  ['/account/devices', 'account', 'DevicesPage', 'user', 'browser'],
  ['/account/notifications', 'account', 'NotificationsPage', 'user', 'browser'],
  ['/admin', 'admin', 'DashboardPage', 'admin', 'browser'],
  ['/admin/users', 'admin', 'UsersPage', 'admin', 'browser'],
  ['/admin/approvals', 'admin', 'ApprovalsPage', 'admin', 'browser'],
  ['/admin/invites', 'admin', 'InvitesPage', 'inviter', 'browser'],
  ['/admin/rooms', 'admin', 'RoomsPage', 'admin', 'browser'],
  ['/admin/settings', 'admin', 'SettingsPage', 'admin', 'browser'],
  ['/admin/audit', 'admin', 'AuditPage', 'admin', 'browser'],
  ['/admin/doctor', 'admin', 'DoctorPage', 'admin', 'browser'],
  // M2: the device approval page (02 §10.1), a page of the browser only.
  ['/link', 'link', 'DeviceLinkPage', 'user', 'browser'],
  // M2: the desktop app's own pages (docs/m2/03 §7.5).
  ['/desktop/setup', 'desktop', 'SetupPage', 'public', 'desktop'],
  ['/desktop/relink', 'desktop', 'RelinkPage', 'public', 'desktop'],
  ['/desktop/settings', 'desktop', 'SettingsPage', 'public', 'desktop'],
  ['/no/such/page', null, 'NotFound', 'public', 'both'],
];

function guardOf(routes: RouteObject[]): Access {
  const components = routes.map((r) => r.Component);
  if (components.includes(RequireInviter)) return 'inviter';
  if (components.includes(RequireAdmin)) return 'admin';
  if (components.includes(RequireAuth)) return 'user';
  return 'public';
}

function whereOf(routes: RouteObject[]): Where {
  const components = routes.map((r) => r.Component);
  if (components.includes(RequireBrowser)) return 'browser';
  if (components.includes(RequireDesktop)) return 'desktop';
  return 'both';
}

const TARGETS: RouteTarget[] = ['browser', 'desktop'];

describe('the route table (05 §5; M2: docs/m2/03 §7.5)', () => {
  describe.each(TARGETS)('for the %s', (target) => {
    it.each(TABLE)('%s → %s/%s, access %s, shown in %s', (path, folder, page, access, where) => {
      const matches = matchRoutes(createAppRoutes(none, target), path);
      expect(matches).not.toBeNull();
      const chain = (matches ?? []).map((m) => m.route);
      const leaf = chain.at(-1);
      expect(guardOf(chain)).toBe(access);
      expect(whereOf(chain)).toBe(where);
      if (folder === null) {
        expect(leaf?.Component).toBe(NotFound);
        return;
      }
      expect(leaf?.handle).toEqual({ folder, page });
      if (where === 'both' || where === target) {
        // Every page folder is a lazy chunk, the room too: the route has a loader and no page of its own yet.
        expect(typeof leaf?.lazy).toBe('function');
        expect(leaf?.Component).toBeUndefined();
      } else {
        // The other platform's page: its guard never renders it here, and nothing is loaded for it.
        expect(leaf?.lazy).toBeUndefined();
        expect(leaf?.Component).toBe(NotFound);
      }
    });

    it('has no other page routes than the table, and no dot in any segment (04 §9.5)', () => {
      const leaves: string[] = [];
      const walk = (routes: RouteObject[], prefix: string): void => {
        for (const r of routes) {
          const path = r.path === undefined ? prefix : `${prefix}/${r.path}`.replace(/\/+/g, '/');
          expect(r.path ?? '').not.toContain('.');
          const handle = r.handle as RouteHandle | undefined;
          if (handle) leaves.push(`${handle.folder}/${handle.page}`);
          if (r.children) walk(r.children, path);
        }
      };
      walk(createAppRoutes(none, target), '');
      const expected = TABLE.filter(([, f]) => f !== null).map(([, f, p]) => `${String(f)}/${p}`);
      // AdminLayout wraps the admin pages twice (admins, and inviters on /admin/invites).
      expect(leaves.filter((l) => l !== 'admin/AdminLayout').sort()).toEqual(expected.sort());
      expect(leaves.filter((l) => l === 'admin/AdminLayout')).toHaveLength(2);
    });
  });

  it('is the browser’s table by default', () => {
    for (const path of ['/desktop/setup', '/link']) {
      const def = matchRoutes(createAppRoutes(none), path)?.at(-1)?.route;
      const browser = matchRoutes(createAppRoutes(none, 'browser'), path)?.at(-1)?.route;
      expect(typeof def?.lazy).toBe(typeof browser?.lazy);
      expect(def?.Component).toBe(browser?.Component);
    }
  });

  it('routeTargetOf: desktop only for a platform with the desktop app’s features', () => {
    expect(routeTargetOf(createTestPlatform())).toBe('browser');
    expect(routeTargetOf(createTestPlatform({ desktop: null }))).toBe('browser');
    expect(routeTargetOf(createDesktopTestPlatform().platform)).toBe('desktop');
  });

  it('finds the page folders of this build with import.meta.glob (none exist before the page slices)', () => {
    // defaultSources() is what createAppRoutes() uses; with no folders every route still resolves.
    expect(() => createAppRoutes()).not.toThrow();
    expect(() => createAppRoutes(undefined, 'desktop')).not.toThrow();
  });

  it('has a loader for every folder of this build, rooms/ and desktop/ among them', () => {
    const { lazy } = defaultSources();
    // link/ joins with WA63.
    expect(Object.keys(lazy).sort()).toEqual(['account', 'admin', 'auth', 'desktop', 'download', 'rooms', 'setup']);
    for (const load of Object.values(lazy)) expect(load).toBeTypeOf('function');
  });
});

describe('the room as a lazy folder (05 §5)', () => {
  it('in a build (the `production` mode) the default routes load rooms/ on demand: no folder is there up front', async () => {
    // A fresh copy of the router's module, as a build evaluates it.
    vi.resetModules();
    vi.stubEnv('MODE', 'production');
    try {
      const router = await import('./router');
      expect(router.defaultSources().eager).toBeUndefined();
      for (const path of ['/', '/r/lounge']) {
        const leaf = matchRoutes(router.createAppRoutes(), path)?.at(-1)?.route;
        expect(typeof leaf?.lazy).toBe('function');
        expect(leaf?.Component).toBeUndefined();
      }
    } finally {
      vi.unstubAllEnvs();
      vi.resetModules();
    }
  });

  it('in Vitest the default routes have the real rooms/ from the start, for the tests of rooms/', () => {
    // router.tsx's ROOMS_IN_TESTS: those tests fake the clock and look for the room after one tick.
    const rooms = defaultSources().eager?.rooms;
    expect(Object.keys(rooms ?? {})).toEqual(expect.arrayContaining(['RoomPage', 'RootRedirect', 'InRoomBar']));
    for (const [path, page] of [
      ['/', 'RootRedirect'],
      ['/r/lounge', 'RoomPage'],
    ] as const) {
      const leaf = matchRoutes(createAppRoutes(), path)?.at(-1)?.route;
      expect(leaf?.Component).toBe(rooms?.[page]);
      expect(leaf?.lazy).toBeUndefined();
    }
  });

  /** A rooms/ folder with a page and a bar that says so, and an auth/ folder to be somewhere else. */
  function sources(rooms: () => Promise<FolderModule>): AppRouteSources {
    return {
      lazy: {
        rooms,
        auth: () =>
          Promise.resolve({
            LoginPage: () => <h1>login</h1>,
            AboutPage: () => <h1>about</h1>,
          }),
      },
    };
  }
  const roomsFolder: FolderModule = {
    RoomPage: () => <h1>the room</h1>,
    InRoomBar: () => <p>in a room</p>,
  };

  it('loads rooms/ on the first visit to a room, and not for another page', async () => {
    server.use(signedIn());
    const rooms = vi.fn(() => Promise.resolve(roomsFolder));
    const { router } = renderRoute({ path: '/about', routes: createAppRoutes(sources(rooms)) });
    expect(await screen.findByRole('heading', { name: 'about' })).toBeInTheDocument();
    expect(rooms).not.toHaveBeenCalled();

    await act(() => router.navigate('/r/lounge'));
    expect(await screen.findByRole('heading', { name: 'the room' })).toBeInTheDocument();
    expect(rooms).toHaveBeenCalled();
  });

  it('the layout shows InRoomBar of rooms/ above the pages once rooms/ is loaded, and nothing before', async () => {
    server.use(signedIn());
    const rooms = vi.fn(() => Promise.resolve(roomsFolder));
    const { router } = renderRoute({ path: '/about', routes: createAppRoutes(sources(rooms)) });
    const about = await screen.findByRole('heading', { name: 'about' });
    // No room was opened in this page load: there is nothing to show, and nothing was fetched to show it.
    expect(screen.queryByText('in a room')).not.toBeInTheDocument();
    expect(about.parentElement?.children).toHaveLength(1);

    await act(() => router.navigate('/r/lounge'));
    expect(await screen.findByRole('heading', { name: 'the room' })).toBeInTheDocument();
    // The bar decides for itself what to show where (rooms/InRoomBar.tsx); the layout has it from here on.
    expect(screen.getByText('in a room')).toBeInTheDocument();

    await act(() => router.navigate('/about'));
    const again = await screen.findByRole('heading', { name: 'about' });
    const bar = screen.getByText('in a room');
    // Above the page, not inside it.
    expect(bar.compareDocumentPosition(again) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(again.contains(bar)).toBe(false);
  });

  it('a rooms/ without an InRoomBar export (a stand-in) leaves the layout as it is', async () => {
    server.use(signedIn());
    const rooms = (): Promise<FolderModule> => Promise.resolve({ RoomPage: () => <h1>the room</h1> });
    renderRoute({ path: '/r/lounge', routes: createAppRoutes(sources(rooms)) });
    const room = await screen.findByRole('heading', { name: 'the room' });
    expect(room.parentElement?.children).toHaveLength(1);
  });

  it('shows the route error screen when rooms/ fails to load, like any folder', async () => {
    clearLog();
    server.use(signedIn());
    renderRoute({
      path: '/r/lounge',
      routes: createAppRoutes(sources(() => Promise.reject(new Error('chunk 404')))),
    });
    expect(await screen.findByRole('heading', { name: 'Something went wrong' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Reload' })).toBeInTheDocument();
  });
});

describe('preloadRoute: the first page’s folders, ahead of the router', () => {
  function spies() {
    const calls: string[] = [];
    const folder = (name: string) => () => {
      calls.push(name);
      return Promise.resolve({});
    };
    const sources: AppRouteSources = {
      lazy: {
        rooms: folder('rooms'),
        auth: folder('auth'),
        setup: folder('setup'),
        account: folder('account'),
        admin: folder('admin'),
        download: folder('download'),
        link: folder('link'),
        desktop: folder('desktop'),
      },
    };
    return { calls, sources };
  }

  it.each([
    ['/', ['rooms']],
    ['/r/lounge', ['rooms']],
    ['/login', ['auth']],
    ['/invite', ['auth']],
    ['/setup', ['setup']],
    ['/admin/welcome', ['setup']],
    ['/account/devices', ['account']],
    ['/download', ['download']],
    // The layout and the page are one folder: asked for once.
    ['/admin/users', ['admin']],
    ['/admin/invites', ['admin']],
    ['/link', ['link']],
    // The desktop app's pages are never loaded by a browser.
    ['/desktop/setup', []],
    ['/desktop/settings', []],
    // No page folder behind this.
    ['/no/such/page', []],
  ])('%s asks for %j', (path, folders) => {
    const { calls, sources } = spies();
    preloadRoute(path, sources);
    expect(calls).toEqual(folders);
  });

  it.each([
    ['/', ['rooms']],
    ['/r/lounge', ['rooms']],
    ['/about', ['auth']],
    ['/desktop/setup', ['desktop']],
    ['/desktop/relink', ['desktop']],
    ['/desktop/settings', ['desktop']],
    // The browser's pages open in the system browser: nothing of them is loaded in the app.
    ['/link', []],
    ['/login', []],
    ['/setup', []],
    ['/account', []],
    ['/admin/users', []],
    ['/download', []],
  ])('in the desktop app, %s asks for %j', (path, folders) => {
    const { calls, sources } = spies();
    preloadRoute(path, sources, 'desktop');
    expect(calls).toEqual(folders);
  });

  it('asks for exactly what the router then loads for that path', async () => {
    server.use(signedIn(meFixture({ admin: true })));
    const { calls, sources } = spies();
    preloadRoute('/admin/users', sources);
    const asked = [...calls];
    calls.length = 0;
    renderRoute({ path: '/admin/users', routes: createAppRoutes(sources) });
    expect(await screen.findByRole('heading', { name: 'Not in this build yet' })).toBeInTheDocument();
    expect([...new Set(calls)]).toEqual(asked);
  });

  it('skips a folder that is there already, and one that this build does not have', () => {
    const { calls, sources } = spies();
    preloadRoute('/r/lounge', { ...sources, eager: { rooms: {} } });
    preloadRoute('/login', { lazy: {} });
    expect(calls).toEqual([]);
  });

  it('leaves a folder that cannot be loaded to the router: the failure is taken, not thrown', async () => {
    const failed = Promise.reject<FolderModule>(new Error('chunk 404'));
    const taken = vi.spyOn(failed, 'catch');
    expect(() => {
      preloadRoute('/r/lounge', { lazy: { rooms: () => failed } });
    }).not.toThrow();
    // Without it, the rejection would be an unhandled one (which also fails the test run).
    expect(taken).toHaveBeenCalledOnce();
    await expect(failed).rejects.toThrow('chunk 404');
  });
});

describe('lazy page folders', () => {
  it('loads a folder when one of its routes is visited, and picks the page by export name', async () => {
    const auth = vi.fn(() =>
      Promise.resolve({
        LoginPage: () => <h1>login form</h1>,
        // InvitePage not yet exported
      }),
    );
    const sources: AppRouteSources = { lazy: { auth } };
    const { router } = renderRoute({ path: '/login', routes: createAppRoutes(sources) });
    expect(await screen.findByRole('heading', { name: 'login form' })).toBeInTheDocument();
    expect(auth).toHaveBeenCalled();

    await act(() => router.navigate('/invite'));
    expect(await screen.findByRole('heading', { name: 'Not in this build yet' })).toBeInTheDocument();
  });

  it('renders PageUnavailable for a folder that does not exist yet', async () => {
    renderRoute({ path: '/download', routes: createAppRoutes(none) });
    expect(await screen.findByRole('heading', { name: 'Not in this build yet' })).toBeInTheDocument();
  });

  it('renders a folder that is there already without loading it (the tests’ stand-in for the room)', async () => {
    server.use(signedIn());
    const load = vi.fn(() => Promise.resolve({ RoomPage: () => <h1>loaded</h1> }));
    const sources: AppRouteSources = {
      lazy: { rooms: load },
      eager: { rooms: { RoomPage: () => <h1>the room</h1> } },
    };
    const routes = createAppRoutes(sources);
    const leaf = matchRoutes(routes, '/r/lounge')?.at(-1)?.route;
    expect(leaf?.lazy).toBeUndefined();
    expect(leaf?.handle).toEqual({ folder: 'rooms', page: 'RoomPage' });
    renderRoute({ path: '/r/lounge', routes });
    expect(await screen.findByRole('heading', { name: 'the room' })).toBeInTheDocument();
    expect(load).not.toHaveBeenCalled();
  });

  it('wraps admin pages in AdminLayout once the folder has it, and renders them bare before', async () => {
    server.use(signedIn(meFixture({ admin: true })));
    const withLayout: AppRouteSources = {
      lazy: {
        admin: () =>
          Promise.resolve({
            AdminLayout: () => (
              <div>
                <nav aria-label="admin" />
                <Outlet />
              </div>
            ),
            UsersPage: () => <h1>users</h1>,
          }),
      },
    };
    renderRoute({ path: '/admin/users', routes: createAppRoutes(withLayout) });
    expect(await screen.findByRole('heading', { name: 'users' })).toBeInTheDocument();
    expect(screen.getByRole('navigation', { name: 'admin' })).toBeInTheDocument();
  });

  it('renders the child page when the folder has no AdminLayout yet', async () => {
    server.use(signedIn(meFixture({ admin: true })));
    const noLayout: AppRouteSources = {
      lazy: { admin: () => Promise.resolve({ UsersPage: () => <h1>users</h1> }) },
    };
    renderRoute({ path: '/admin/users', routes: createAppRoutes(noLayout) });
    expect(await screen.findByRole('heading', { name: 'users' })).toBeInTheDocument();
  });

  it('shows the route error screen when a folder fails to load (a stale chunk after an update)', async () => {
    clearLog();
    const sources: AppRouteSources = { lazy: { auth: () => Promise.reject(new Error('chunk 404')) } };
    renderRoute({ path: '/login', routes: createAppRoutes(sources) });
    expect(await screen.findByRole('heading', { name: 'Something went wrong' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Reload' })).toBeInTheDocument();
    await waitFor(() => {
      expect(logLines().some((l) => l.level === 'error' && l.msg === 'route render failed')).toBe(true);
    });
  });

  it('moves focus to the new page heading after navigating (05 §16.6)', async () => {
    const sources: AppRouteSources = {
      lazy: {
        auth: () =>
          Promise.resolve({
            LoginPage: () => (
              <main>
                <h1>login</h1>
                <Link to="/pending">pending</Link>
              </main>
            ),
            PendingPage: () => (
              <main>
                <h1>pending</h1>
              </main>
            ),
          }),
      },
    };
    const { router } = renderRoute({ path: '/login', routes: createAppRoutes(sources) });
    const first = await screen.findByRole('heading', { name: 'login' });
    expect(first).not.toHaveFocus(); // not on the first load
    await act(() => router.navigate('/pending'));
    const heading = await screen.findByRole('heading', { name: 'pending' });
    await waitFor(() => {
      expect(heading).toHaveFocus();
    });
  });
});

describe('guards (05 §5)', () => {
  const pages: AppRouteSources = {
    lazy: {
      account: () => Promise.resolve({ AccountPage: () => <h1>account</h1> }),
      admin: () =>
        Promise.resolve({
          InvitesPage: () => <h1>invites</h1>,
          UsersPage: () => <h1>users</h1>,
          DashboardPage: () => <h1>dash</h1>,
        }),
      auth: () => Promise.resolve({ LoginPage: () => <h1>login</h1> }),
    },
    eager: { rooms: { RootRedirect: () => <h1>root</h1> } },
  };

  it('RequireAuth sends signed-out users to /login?next=<path>', async () => {
    server.use(signedIn(null));
    const { router } = renderRoute({ path: '/account?tab=pw', routes: createAppRoutes(pages) });
    expect(await screen.findByRole('heading', { name: 'login' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/login');
    expect(router.state.location.search).toBe(`?next=${encodeURIComponent('/account?tab=pw')}`);
  });

  it('RequireAuth sends / to plain /login', async () => {
    server.use(signedIn(null));
    const { router } = renderRoute({ path: '/', routes: createAppRoutes(pages) });
    expect(await screen.findByRole('heading', { name: 'login' })).toBeInTheDocument();
    expect(router.state.location.search).toBe('');
  });

  it('RequireAuth renders the page for a signed-in user', async () => {
    server.use(signedIn());
    renderRoute({ path: '/account', routes: createAppRoutes(pages) });
    expect(await screen.findByRole('heading', { name: 'account' })).toBeInTheDocument();
  });

  it('RequireAdmin shows NotFound to members, so admin pages are not advertised', async () => {
    server.use(signedIn(meFixture({ admin: false, createInvites: true })));
    renderRoute({ path: '/admin/users', routes: createAppRoutes(pages) });
    expect(await screen.findByRole('heading', { name: 'Nothing here' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'users' })).not.toBeInTheDocument();
  });

  it('RequireAdmin lets admins in', async () => {
    server.use(signedIn(meFixture({ admin: true })));
    renderRoute({ path: '/admin', routes: createAppRoutes(pages) });
    expect(await screen.findByRole('heading', { name: 'dash' })).toBeInTheDocument();
  });

  it.each([
    ['a member with createInvites', meFixture({ admin: false, createInvites: true }), 'invites'],
    ['an admin', meFixture({ admin: true }), 'invites'],
    ['a member without createInvites', meFixture({ admin: false, createInvites: false }), 'Nothing here'],
  ])('RequireInviter on /admin/invites: %s → %s', async (_who, me, heading) => {
    server.use(signedIn(me));
    renderRoute({ path: '/admin/invites', routes: createAppRoutes(pages) });
    expect(await screen.findByRole('heading', { name: heading })).toBeInTheDocument();
  });

  it('public routes never ask for /me', async () => {
    let asked = false;
    server.use(
      http.get(apiPath('/api/v1/me'), () => {
        asked = true;
        return HttpResponse.json(meFixture());
      }),
    );
    renderRoute({ path: '/login', routes: createAppRoutes(pages) });
    expect(await screen.findByRole('heading', { name: 'login' })).toBeInTheDocument();
    expect(asked).toBe(false);
  });

  it('shows the offline screen with Try now when /me cannot be reached', async () => {
    server.use(http.get(apiPath('/api/v1/me'), () => HttpResponse.error()));
    renderRoute({ path: '/account', routes: createAppRoutes(pages) });
    expect(await screen.findByRole('heading', { name: "Can't reach the server" })).toBeInTheDocument();
    server.use(signedIn());
    await userEvent.click(screen.getByRole('button', { name: 'Try now' }));
    expect(await screen.findByRole('heading', { name: 'account' })).toBeInTheDocument();
  });

  it('a 401 from any query while signed in sends the user to /login (["me"] cleared)', async () => {
    server.use(signedIn());
    const { router, services } = renderRoute({ path: '/account', routes: createAppRoutes(pages) });
    expect(await screen.findByRole('heading', { name: 'account' })).toBeInTheDocument();
    // The session ends on the server; the next request of any query gets 401 unauthenticated.
    server.use(
      signedIn(null),
      http.get(apiPath('/api/v1/rooms'), () => apiError(401, { code: 'unauthenticated' })),
    );
    await act(async () => {
      await services.queryClient
        .query({ queryKey: queryKeys.rooms, queryFn: () => api('GET', '/api/v1/rooms') })
        .catch(() => undefined);
    });
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/login');
    });
  });
});

describe('M2: the desktop app’s routes and guards (docs/m2/03 §7.5, §22.3)', () => {
  /**
   * Every folder: each page says which it is, and `loaded` says which folders were fetched. rooms/ is there
   * already, as in Vitest's default sources.
   */
  function folders() {
    const loaded: string[] = [];
    const heading = (text: string) => () => <h1>{text}</h1>;
    const folder = (name: string, pages: string[]) => () => {
      loaded.push(name);
      return Promise.resolve(Object.fromEntries(pages.map((p) => [p, heading(`${name}/${p}`)])));
    };
    const sources: AppRouteSources = {
      lazy: {
        auth: folder('auth', ['LoginPage', 'InvitePage', 'SignupPage', 'PendingPage', 'ResetPage', 'AboutPage']),
        setup: folder('setup', ['SetupPage', 'WelcomePage']),
        account: folder('account', ['AccountPage', 'DevicesPage', 'NotificationsPage']),
        admin: folder('admin', [
          'DashboardPage',
          'UsersPage',
          'ApprovalsPage',
          'InvitesPage',
          'RoomsPage',
          'SettingsPage',
          'AuditPage',
          'DoctorPage',
        ]),
        download: folder('download', ['DownloadPage']),
        link: folder('link', ['DeviceLinkPage']),
        desktop: folder('desktop', ['SetupPage', 'RelinkPage', 'SettingsPage']),
      },
      eager: { rooms: { RootRedirect: heading('rooms/RootRedirect'), RoomPage: heading('rooms/RoomPage') } },
    };
    return { loaded, sources };
  }

  /** Counts GET /api/v1/me; answers with `me` (null: signed out). */
  function meRequests(me: () => ReturnType<typeof meFixture> | null = () => null) {
    const asked = { count: 0 };
    server.use(
      http.get(apiPath('/api/v1/me'), () => {
        asked.count++;
        const m = me();
        return m === null ? apiError(401, { code: 'unauthenticated' }) : HttpResponse.json(m);
      }),
    );
    return asked;
  }

  function renderDesktop(path: string, d = createDesktopTestPlatform(), sources = folders().sources) {
    return {
      ...renderRoute({
        path,
        routes: createAppRoutes(sources, 'desktop'),
        services: createTestServices({ platform: d.platform }),
      }),
      d,
    };
  }

  const BROWSER_PAGES = TABLE.filter(([, , , , where]) => where === 'browser').map(([path]) => path);
  const DESKTOP_PAGES = TABLE.filter(([, , , , where]) => where === 'desktop');

  describe('in the browser', () => {
    it.each(DESKTOP_PAGES)('%s: RequireDesktop shows NotFound, and desktop/ is never loaded', async (path) => {
      const { loaded, sources } = folders();
      renderRoute({ path, routes: createAppRoutes(sources, 'browser') });
      expect(await screen.findByRole('heading', { name: 'Nothing here' })).toBeInTheDocument();
      expect(loaded).toEqual([]);
    });

    it.each(['/desktop', '/desktop/'])('%s: NotFound, and desktop/ is never loaded', async (path) => {
      const { loaded, sources } = folders();
      renderRoute({ path, routes: createAppRoutes(sources, 'browser') });
      expect(await screen.findByRole('heading', { name: 'Nothing here' })).toBeInTheDocument();
      expect(loaded).toEqual([]);
    });

    it('RequireDesktop on its own: NotFound for a platform without desktop features, even with the page loaded', async () => {
      const { sources } = folders();
      renderRoute({ path: '/desktop/settings', routes: createAppRoutes(sources, 'desktop') });
      expect(await screen.findByRole('heading', { name: 'Nothing here' })).toBeInTheDocument();
      expect(screen.queryByRole('heading', { name: 'desktop/SettingsPage' })).not.toBeInTheDocument();
    });

    it('RequireBrowser renders the page and opens nothing (/link among them)', async () => {
      server.use(signedIn());
      const openExternal = vi.fn();
      const { loaded, sources } = folders();
      renderRoute({
        path: '/link?code=WDJB-MJHT',
        routes: createAppRoutes(sources, 'browser'),
        services: createTestServices({ platform: createTestPlatform({ openExternal }) }),
      });
      expect(await screen.findByRole('heading', { name: 'link/DeviceLinkPage' })).toBeInTheDocument();
      expect(loaded).toEqual(['link']);
      expect(openExternal).not.toHaveBeenCalled();
    });
  });

  describe('in the desktop app', () => {
    it.each(DESKTOP_PAGES)('%s → %s/%s from the desktop/ folder', async (path, _folder, page) => {
      const { loaded, sources } = folders();
      renderDesktop(path, undefined, sources);
      expect(await screen.findByRole('heading', { name: `desktop/${page}` })).toBeInTheDocument();
      expect(loaded).toEqual(['desktop']);
    });

    it.each(['/desktop', '/desktop/'])(
      '%s is no page: NotFound (not an empty layout), and nothing is loaded',
      async (path) => {
        const { loaded, sources } = folders();
        renderDesktop(path, undefined, sources);
        expect(await screen.findByRole('heading', { name: 'Nothing here' })).toBeInTheDocument();
        expect(loaded).toEqual([]);
      },
    );

    it('the real desktop/ folder has every page: WA42’s stubs, "Not in this build yet"', async () => {
      const { router } = renderDesktop('/desktop/setup', undefined, defaultSources());
      expect(await screen.findByRole('heading', { name: 'Not in this build yet' })).toBeInTheDocument();
      for (const path of ['/desktop/relink', '/desktop/settings']) {
        await act(() => router.navigate(path));
        expect(await screen.findByRole('heading', { name: 'Not in this build yet' })).toBeInTheDocument();
      }
      const mod = await defaultSources().lazy.desktop?.();
      expect(Object.keys(mod ?? {}).sort()).toEqual(['RelinkPage', 'SettingsPage', 'SetupPage']);
    });

    it.each(BROWSER_PAGES)(
      '%s: RequireBrowser opens the browser once and loads nothing, not even ["me"]',
      async (path) => {
        const asked = meRequests(() => meFixture({ admin: true }));
        const { loaded, sources } = folders();
        const { d } = renderDesktop(`${path}?x=1`, undefined, sources);
        expect(await screen.findByRole('heading', { name: 'Opened in your browser' })).toBeInTheDocument();
        expect(screen.getByRole('button', { name: 'Back' })).toBeInTheDocument();
        expect(d.opened).toEqual([`${d.platform.serverOrigin}${path}?x=1`]);
        expect(loaded).toEqual([]);
        expect(asked.count).toBe(0);
      },
    );

    it('/link keeps its ?code= in the browser’s URL, and link/ is never loaded', async () => {
      const { loaded, sources } = folders();
      const { d } = renderDesktop('/link?code=WDJB-MJHT', undefined, sources);
      expect(await screen.findByRole('heading', { name: 'Opened in your browser' })).toBeInTheDocument();
      expect(d.opened).toEqual([`${d.platform.serverOrigin}/link?code=WDJB-MJHT`]);
      expect(loaded).toEqual([]);
      preloadRoute('/link?code=WDJB-MJHT', sources, 'desktop');
      expect(loaded).toEqual([]);
    });

    it('opens once per visit: StrictMode and re-renders don’t open it again; a new visit does', async () => {
      server.use(signedIn());
      const d = createDesktopTestPlatform();
      const services = createTestServices({ platform: d.platform });
      const router = createMemoryRouter(createAppRoutes(folders().sources, 'desktop'), {
        initialEntries: ['/account'],
      });
      const view = (
        <StrictMode>
          <AppProviders services={services}>
            <RouterProvider router={router} />
          </AppProviders>
        </StrictMode>
      );
      const { rerender } = render(view);
      expect(await screen.findByRole('heading', { name: 'Opened in your browser' })).toBeInTheDocument();
      rerender(view);
      expect(d.opened).toEqual([`${d.platform.serverOrigin}/account`]);

      await act(() => router.navigate('/r/lounge'));
      expect(await screen.findByRole('heading', { name: 'rooms/RoomPage' })).toBeInTheDocument();
      await act(() => router.navigate('/account'));
      expect(await screen.findByRole('heading', { name: 'Opened in your browser' })).toBeInTheDocument();
      await act(() => router.navigate('/admin/users'));
      expect(await screen.findByRole('heading', { name: 'Opened in your browser' })).toBeInTheDocument();
      expect(d.opened).toEqual([
        `${d.platform.serverOrigin}/account`,
        `${d.platform.serverOrigin}/account`,
        `${d.platform.serverOrigin}/admin/users`,
      ]);
    });

    it('Back returns to the app’s page before, or to the app’s start when it opened there', async () => {
      server.use(signedIn());
      const { router } = renderDesktop('/r/lounge');
      expect(await screen.findByRole('heading', { name: 'rooms/RoomPage' })).toBeInTheDocument();
      await act(() => router.navigate('/account/devices'));
      await userEvent.click(await screen.findByRole('button', { name: 'Back' }));
      expect(await screen.findByRole('heading', { name: 'rooms/RoomPage' })).toBeInTheDocument();
      expect(router.state.location.pathname).toBe('/r/lounge');
    });

    it('Back from the first page goes to the start', async () => {
      server.use(signedIn());
      const { router } = renderDesktop('/download');
      await userEvent.click(await screen.findByRole('button', { name: 'Back' }));
      expect(await screen.findByRole('heading', { name: 'rooms/RootRedirect' })).toBeInTheDocument();
      expect(router.state.location.pathname).toBe('/');
    });

    it('/about works in the desktop app too', async () => {
      const { loaded, sources } = folders();
      const { d } = renderDesktop('/about', undefined, sources);
      expect(await screen.findByRole('heading', { name: 'auth/AboutPage' })).toBeInTheDocument();
      expect(loaded).toEqual(['auth']);
      expect(d.opened).toEqual([]);
    });

    it('without a server there is nothing to open: NotFound', async () => {
      const d = createDesktopTestPlatform({ server: null });
      renderDesktop('/login', d);
      expect(await screen.findByRole('heading', { name: 'Nothing here' })).toBeInTheDocument();
      expect(d.opened).toEqual([]);
    });
  });

  describe('RequireAuth in the desktop app: by the AuthState, never /login', () => {
    it.each<[AuthState['status'], AuthNotice, string]>([
      ['unlinked', '', '/desktop/setup'],
      ['unlinked', 'signed_out', '/desktop/relink'],
      ['unlinked', 'refresh_reused', '/desktop/relink'],
      ['unlinked', 'account_disabled', '/desktop/relink'],
      // The link step's own outcomes: the wizard shows them.
      ['unlinked', 'denied', '/desktop/setup'],
      ['unlinked', 'code_expired', '/desktop/setup'],
      ['linking', '', '/desktop/setup'],
      // The link step shows it with Try again (Auth.Recheck).
      ['incompatible', '', '/desktop/setup'],
    ])('%s with notice %j → %s', async (status, notice, path) => {
      meRequests();
      const d = createDesktopTestPlatform();
      d.setAuth(status, notice);
      const { router } = renderDesktop('/r/lounge', d);
      const page = path === '/desktop/relink' ? 'desktop/RelinkPage' : 'desktop/SetupPage';
      expect(await screen.findByRole('heading', { name: page })).toBeInTheDocument();
      expect(router.state.location.pathname).toBe(path);
      expect(d.fake.callsOf('auth.state')).toHaveLength(1);
    });

    it.each<AuthState['status']>(['linked', 'offline'])(
      '%s: waits, and asks for ["me"] again after the next auth.state',
      async (status) => {
        let me: ReturnType<typeof meFixture> | null = null;
        const asked = meRequests(() => me);
        const d = createDesktopTestPlatform();
        d.setAuth(status);
        const { router } = renderDesktop('/r/lounge', d);
        await waitFor(() => {
          expect(d.fake.listeners('auth.state')).toBe(1);
        });
        await waitFor(() => {
          expect(d.fake.callsOf('auth.state')).toHaveLength(1);
        });
        expect(asked.count).toBe(1);
        expect(router.state.location.pathname).toBe('/r/lounge');
        expect(screen.queryByRole('heading')).not.toBeInTheDocument();

        // Go refreshed the token: the state comes again, and ['me'] answers now.
        me = meFixture();
        act(() => {
          d.fake.emit('auth.state', authState(d.platform.serverOrigin, 'linked'));
        });
        expect(await screen.findByRole('heading', { name: 'rooms/RoomPage' })).toBeInTheDocument();
        expect(asked.count).toBe(2);
        expect(router.state.location.pathname).toBe('/r/lounge');
      },
    );

    it('a later auth.state decides too: linked, then signed out → /desktop/relink', async () => {
      meRequests();
      const d = createDesktopTestPlatform();
      d.setAuth('linked');
      const { router } = renderDesktop('/', d);
      await waitFor(() => {
        expect(d.fake.callsOf('auth.state')).toHaveLength(1);
      });
      act(() => {
        d.fake.emit('auth.state', { ...authState(d.platform.serverOrigin, 'unlinked'), notice: 'signed_out' });
      });
      expect(await screen.findByRole('heading', { name: 'desktop/RelinkPage' })).toBeInTheDocument();
      expect(router.state.location.pathname).toBe('/desktop/relink');
    });

    it('ignores an auth.state of another server: the window reloads for it', async () => {
      const asked = meRequests();
      const d = createDesktopTestPlatform();
      d.setAuth('linked');
      const { router } = renderDesktop('/r/lounge', d);
      await waitFor(() => {
        expect(d.fake.callsOf('auth.state')).toHaveLength(1);
      });
      act(() => {
        d.fake.emit('auth.state', { ...authState('https://other.example', 'unlinked'), notice: 'signed_out' });
        d.fake.emit('auth.state', authState('https://other.example', 'linked'));
      });
      await act(() => Promise.resolve());
      expect(router.state.location.pathname).toBe('/r/lounge');
      expect(asked.count).toBe(1);
    });

    it('a state of another server from Auth.State() is ignored as well', async () => {
      meRequests();
      const d = createDesktopTestPlatform();
      d.fake.answers['auth.state'] = { ...authState('https://other.example', 'unlinked'), notice: 'signed_out' };
      const { router } = renderDesktop('/r/lounge', d);
      await waitFor(() => {
        expect(d.fake.callsOf('auth.state')).toHaveLength(1);
      });
      await act(() => Promise.resolve());
      expect(router.state.location.pathname).toBe('/r/lounge');
    });

    it('Auth.State() failing: the Fatal screen with its code', async () => {
      meRequests();
      const d = createDesktopTestPlatform();
      d.fake.answers['auth.state'] = () => Promise.reject(new BridgeError('not_implemented'));
      renderDesktop('/r/lounge', d);
      expect(await screen.findByRole('heading', { name: 'Something went wrong' })).toBeInTheDocument();
      expect(screen.getByText('Error: not_implemented')).toBeInTheDocument();
    });

    it('before the first server: the wizard, without asking for ["me"] or the bridge', async () => {
      const asked = meRequests();
      const d = createDesktopTestPlatform({ server: null });
      const { router } = renderDesktop('/r/lounge', d);
      expect(await screen.findByRole('heading', { name: 'desktop/SetupPage' })).toBeInTheDocument();
      expect(router.state.location.pathname).toBe('/desktop/setup');
      expect(asked.count).toBe(0);
      expect(d.fake.calls).toEqual([]);
    });

    it('an incompatible active server: Go gives no token, so ["me"] is null with no request sent, and the link step shows why', async () => {
      // The app's own DesktopPlatform: its apiFetch asks Auth.AccessToken first, which rejects in this state
      // (docs/m2/03 §6.2), so /me is never sent and no 401 comes back.
      const asked = meRequests(() => meFixture());
      const d = createDesktopTestPlatform({ real: true });
      d.setAuth('incompatible');
      d.fake.answers['auth.accessToken'] = () =>
        Promise.reject(new BridgeError('incompatible_server', { verdict: 'update_app' }));
      const { router } = renderDesktop('/r/lounge', d);
      expect(await screen.findByRole('heading', { name: 'desktop/SetupPage' })).toBeInTheDocument();
      expect(router.state.location.pathname).toBe('/desktop/setup');
      expect(asked.count).toBe(0);
      expect(d.fake.callsOf('auth.accessToken')).toEqual([[d.platform.serverOrigin, false]]);
      expect(screen.queryByRole('heading', { name: "Can't reach the server" })).not.toBeInTheDocument();
    });

    it('…while Go unreachable (server_unreachable) is still the Offline screen', async () => {
      const asked = meRequests(() => meFixture());
      const d = createDesktopTestPlatform({ real: true });
      d.fake.answers['auth.accessToken'] = () => Promise.reject(new BridgeError('server_unreachable'));
      renderDesktop('/r/lounge', d);
      expect(await screen.findByRole('heading', { name: "Can't reach the server" })).toBeInTheDocument();
      expect(asked.count).toBe(0);
      expect(d.fake.callsOf('auth.state')).toEqual([]);
    });

    it('the real DesktopPlatform, linked: /me goes with the bearer, and the page shows', async () => {
      const auth: (string | null)[] = [];
      server.use(
        http.get(apiPath('/api/v1/me'), ({ request }) => {
          auth.push(request.headers.get('Authorization'));
          return HttpResponse.json(meFixture());
        }),
      );
      const d = createDesktopTestPlatform({ real: true });
      renderDesktop('/r/lounge', d);
      expect(await screen.findByRole('heading', { name: 'rooms/RoomPage' })).toBeInTheDocument();
      expect(auth).toEqual([`Bearer ${d.fake.issued[0] ?? ''}`]);
      expect(d.fake.issued).toHaveLength(1);
    });

    it('a signed-in user gets the page, and the bridge is not asked', async () => {
      server.use(signedIn());
      const d = createDesktopTestPlatform();
      renderDesktop('/r/lounge', d);
      expect(await screen.findByRole('heading', { name: 'rooms/RoomPage' })).toBeInTheDocument();
      expect(d.fake.callsOf('auth.state')).toEqual([]);
    });

    it('Offline gains "Switch server", to Settings → Servers', async () => {
      server.use(http.get(apiPath('/api/v1/me'), () => HttpResponse.error()));
      const { router } = renderDesktop('/r/lounge');
      expect(await screen.findByRole('heading', { name: "Can't reach the server" })).toBeInTheDocument();
      await userEvent.click(screen.getByRole('button', { name: 'Switch server' }));
      expect(await screen.findByRole('heading', { name: 'desktop/SettingsPage' })).toBeInTheDocument();
      expect(router.state.location).toMatchObject({ pathname: '/desktop/settings', hash: '#servers' });
    });

    it.each<[AuthState['status'], AuthNotice, string | null]>([
      ['unlinked', '', '/desktop/setup'],
      ['unlinked', 'signed_out', '/desktop/relink'],
      ['unlinked', 'refresh_reused', '/desktop/relink'],
      ['unlinked', 'account_disabled', '/desktop/relink'],
      ['unlinked', 'denied', '/desktop/setup'],
      ['unlinked', 'code_expired', '/desktop/setup'],
      ['unlinked', 'save_failed', '/desktop/setup'],
      ['linking', '', '/desktop/setup'],
      ['incompatible', '', '/desktop/setup'],
      ['linked', '', null],
      ['linked', 'save_failed', null],
      ['offline', '', null],
    ])('desktopSignedOutPath(%s, %j) = %s', (status, notice, want) => {
      expect(desktopSignedOutPath({ status, notice })).toBe(want);
    });
  });
});

describe('safeNext and loginPath', () => {
  it.each([
    [null, '/'],
    [undefined, '/'],
    ['', '/'],
    ['/r/lounge?focus=s_1', '/r/lounge?focus=s_1'],
    ['/admin/users', '/admin/users'],
    ['//evil.example/x', '/'],
    ['/\\evil.example', '/'],
    ['https://evil.example/', '/'],
    ['javascript:alert(1)', '/'],
    ['r/lounge', '/'],
    ['/a\nb', '/'],
    ['/a\u0000', '/'],
  ])('safeNext(%j) → %s', (raw, want) => {
    expect(safeNext(raw)).toBe(want);
  });

  it('builds /login?next= from the current path and query', () => {
    expect(loginPath({ pathname: '/r/lounge', search: '?focus=s_1' })).toBe('/login?next=%2Fr%2Flounge%3Ffocus%3Ds_1');
    expect(loginPath({ pathname: '/', search: '' })).toBe('/login');
    expect(safeNext(decodeURIComponent('%2Fr%2Flounge%3Ffocus%3Ds_1'))).toBe('/r/lounge?focus=s_1');
  });
});
