import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { Link, matchRoutes, Outlet, type RouteObject } from 'react-router';

import { clearLog, logLines } from '../lib/log';
import { queryKeys } from '../protocol/queryKeys';
import { api } from '../protocol/rest';
import { apiError, apiPath, meFixture, server, signedIn } from '../test/msw';
import { renderRoute } from '../test/render';
import { RequireAdmin, RequireAuth, RequireInviter, safeNext, loginPath } from './guards';
import { NotFound } from './NotFound';
import {
  createAppRoutes,
  defaultSources,
  preloadRoute,
  type AppRouteSources,
  type FolderModule,
  type RouteHandle,
} from './router';

const none: AppRouteSources = { lazy: {} };

/** 05 §5, row by row: path → [folder, page, access]. */
const TABLE: [string, RouteHandle['folder'] | null, string, 'public' | 'user' | 'admin' | 'inviter'][] = [
  ['/', 'rooms', 'RootRedirect', 'user'],
  ['/r/lounge', 'rooms', 'RoomPage', 'user'],
  ['/login', 'auth', 'LoginPage', 'public'],
  ['/invite', 'auth', 'InvitePage', 'public'],
  ['/signup', 'auth', 'SignupPage', 'public'],
  ['/pending', 'auth', 'PendingPage', 'public'],
  ['/reset', 'auth', 'ResetPage', 'public'],
  ['/setup', 'setup', 'SetupPage', 'public'],
  ['/admin/welcome', 'setup', 'WelcomePage', 'admin'],
  ['/download', 'download', 'DownloadPage', 'public'],
  ['/about', 'auth', 'AboutPage', 'public'],
  ['/account', 'account', 'AccountPage', 'user'],
  ['/account/devices', 'account', 'DevicesPage', 'user'],
  ['/account/notifications', 'account', 'NotificationsPage', 'user'],
  ['/admin', 'admin', 'DashboardPage', 'admin'],
  ['/admin/users', 'admin', 'UsersPage', 'admin'],
  ['/admin/approvals', 'admin', 'ApprovalsPage', 'admin'],
  ['/admin/invites', 'admin', 'InvitesPage', 'inviter'],
  ['/admin/rooms', 'admin', 'RoomsPage', 'admin'],
  ['/admin/settings', 'admin', 'SettingsPage', 'admin'],
  ['/admin/audit', 'admin', 'AuditPage', 'admin'],
  ['/admin/doctor', 'admin', 'DoctorPage', 'admin'],
  ['/link', null, 'NotFound', 'user'],
  ['/no/such/page', null, 'NotFound', 'public'],
];

function guardOf(routes: RouteObject[]): 'public' | 'user' | 'admin' | 'inviter' {
  const components = routes.map((r) => r.Component);
  if (components.includes(RequireInviter)) return 'inviter';
  if (components.includes(RequireAdmin)) return 'admin';
  if (components.includes(RequireAuth)) return 'user';
  return 'public';
}

describe('the route table (05 §5)', () => {
  it.each(TABLE)('%s → %s/%s, access %s', (path, folder, page, access) => {
    const matches = matchRoutes(createAppRoutes(none), path);
    expect(matches).not.toBeNull();
    const chain = (matches ?? []).map((m) => m.route);
    const leaf = chain.at(-1);
    expect(guardOf(chain)).toBe(access);
    if (folder === null) {
      expect(leaf?.Component).toBe(NotFound);
      return;
    }
    expect(leaf?.handle).toEqual({ folder, page });
    // Every page folder is a lazy chunk, the room too: the route has a loader and no page of its own yet.
    expect(typeof leaf?.lazy).toBe('function');
    expect(leaf?.Component).toBeUndefined();
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
    walk(createAppRoutes(none), '');
    const expected = TABLE.filter(([, f]) => f !== null).map(([, f, p]) => `${String(f)}/${p}`);
    // AdminLayout wraps the admin pages twice (admins, and inviters on /admin/invites).
    expect(leaves.filter((l) => l !== 'admin/AdminLayout').sort()).toEqual(expected.sort());
    expect(leaves.filter((l) => l === 'admin/AdminLayout')).toHaveLength(2);
  });

  it('finds the page folders of this build with import.meta.glob (none exist before the page slices)', () => {
    // defaultSources() is what createAppRoutes() uses; with no folders every route still resolves.
    expect(() => createAppRoutes()).not.toThrow();
  });

  it('has a loader for every folder of this build, rooms/ among them', () => {
    const { lazy } = defaultSources();
    expect(Object.keys(lazy).sort()).toEqual(['account', 'admin', 'auth', 'download', 'rooms', 'setup']);
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
    // No page folder behind these.
    ['/link', []],
    ['/no/such/page', []],
  ])('%s asks for %j', (path, folders) => {
    const { calls, sources } = spies();
    preloadRoute(path, sources);
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
