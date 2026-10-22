// Every route of 05 §5 and of M2 (docs/m2/03 §7.5, 02 §10.1), declared once here (README "Shared files": S27 owns
// this file in M1, WA42 in M2; page slices fill only their folder). React Router data router: every page is in a lazy
// chunk, the room too.
//
// Page folders. Each route loads one page component, by name, from its folder's entry module `<folder>/index.ts`
// (or index.tsx), which exports the folder's pages:
//   auth/      LoginPage InvitePage SignupPage PendingPage ResetPage AboutPage      (S33)
//   setup/     SetupPage WelcomePage                                              (S33, S87)
//   account/   AccountPage DevicesPage NotificationsPage                          (S48, S77)
//   admin/     AdminLayout DashboardPage UsersPage ApprovalsPage InvitesPage       (S49, S91)
//              RoomsPage SettingsPage AuditPage DoctorPage
//   download/  DownloadPage                                                       (later)
//   rooms/     RootRedirect RoomPage, and InRoomBar for the layout                 (S34, S45)
//   link/      DeviceLinkPage, the browser's approval of a desktop app            (M2: WA63)
//   desktop/   SetupPage RelinkPage SettingsPage, the desktop app's own pages      (M2: WA42's stubs; WA71, WA82)
// The folders are found with import.meta.glob, so a folder that doesn't exist yet is simply absent: its routes
// render PageUnavailable (a missing AdminLayout renders just its child page; /link renders NotFound, as in M1) until
// the slice adds the export, and no route here changes. A folder is one chunk, loaded on the first visit to one of
// its routes.
//
// The room is a lazy folder like the others (W00): the page, the session and the signaling client are about 24 KB
// gzip, and /login, /invite and /setup, where a friend first arrives, need none of it. Two things go with that:
// - preloadRoute(): boot asks for the folders of the first path while GET /api/v1/info is still on its way (the
//   router, which would ask, is only made once /info has answered), so the first page waits for no round trip of
//   its own;
// - the layout shows rooms/'s InRoomBar (05 §11.1) only once rooms/ is loaded. Before that no page has made the
//   room's runtime, and the bar would render nothing anyway: no page fetches the room's code to show an empty bar.
//
// A lazy folder's texts can come with it: its entry module imports the lazy namespaces its pages use
// (i18n/lazy/<ns>.ts, 05 §16.5), and those are in the catalog once the entry module has run. A route's `lazy`
// below resolves only after that, so a page is never rendered without its texts and the routes need no code for it.
//
// Two route tables, one per RouteTarget (M2, docs/m2/03 §7.5). The browser's and the desktop app's have the same
// paths and guards and differ only in what their routes may load: RequireDesktop renders NotFound in the browser,
// and RequireBrowser hands its pages to the system browser in the desktop app, so a page behind the other platform's
// guard is never shown. In that table it has no `lazy` either, because a data router loads the lazy routes of every
// match before any guard renders: a browser never fetches desktop/, and the desktop app never fetches link/ or any
// other page of the browser.
//
// No route segment contains a dot: 04 §9.5 serves dotted paths as files.
import type { ComponentType } from 'react';
import { matchRoutes, Outlet, type RouteObject } from 'react-router';
import { useStore } from 'zustand';
import { createStore } from 'zustand/vanilla';

import type { Platform } from '../platform/types';
import { RouteErrorBoundary } from './ErrorBoundary';
import { RequireAdmin, RequireAuth, RequireBrowser, RequireDesktop, RequireInviter } from './guards';
import { RootLayout } from './layouts/RootLayout';
import { NotFound, PageUnavailable } from './NotFound';
import { PageSpinner } from '../ui/Spinner';

/** A page folder's entry module: page components by export name. */
export type FolderModule = Readonly<Record<string, unknown>>;

export type PageFolder = 'auth' | 'setup' | 'account' | 'admin' | 'download' | 'rooms' | 'link' | 'desktop';

/** What each route loads, for tests and debugging: route.handle. */
export interface RouteHandle {
  readonly folder: PageFolder;
  readonly page: string;
}

/**
 * Which platform a route table is for (docs/m2/03 §7.5): 'desktop' for the SPA inside the desktop app, 'browser' for
 * every other page.
 */
export type RouteTarget = 'browser' | 'desktop';

/** The route table of a platform: 'desktop' when it has the desktop app's features (platform.desktop). */
export function routeTargetOf(platform: Pick<Platform, 'desktop'>): RouteTarget {
  return (platform.desktop ?? null) !== null ? 'desktop' : 'browser';
}

export interface AppRouteSources {
  /** Loaders of the page folders; a missing folder renders PageUnavailable. */
  lazy: Partial<Record<PageFolder, () => Promise<FolderModule>>>;
  /**
   * Folders that are there without loading, for tests: such a folder's routes have their page from the start (no
   * `lazy`), and its loader is never called. A build has none.
   */
  eager?: Partial<Record<PageFolder, FolderModule>>;
}

/**
 * Vitest only: rooms/, loaded with this module, which defaultSources() then gives as an eager folder.
 *
 * The tests of rooms/ render the app's routes under a faked clock and look for the room after one tick
 * (rooms/testing/page.tsx's advance()). They were written while the room was part of the entry chunk, and a lazy
 * route can't do that: its page is there one render later, and a first import() takes real time besides. So in
 * Vitest the room's routes are as they were. What a build does is tested where the test says which folders there
 * are and in the `production` mode (router.test.tsx), and on the build itself (chunks.node.test.ts). When the
 * tests of rooms/ load the folder themselves and wait for the route, this constant can go.
 *
 * Written as this very comparison on purpose: Vite puts the mode in at build time and the bundler drops the branch,
 * the import() and the await with it (chunks.node.test.ts checks that the entry asks for the room in one place only).
 */
const ROOMS_IN_TESTS: FolderModule | undefined =
  import.meta.env.MODE === 'test' ? await import('../rooms/index') : undefined;

const FOLDER_OF_PATH = /^\.\.\/(\w+)\/index\.tsx?$/;

function byFolder<T>(modules: Record<string, T>): Record<string, T> {
  const out: Record<string, T> = {};
  for (const [file, mod] of Object.entries(modules)) {
    const folder = FOLDER_OF_PATH.exec(file)?.[1];
    if (folder !== undefined) out[folder] = mod;
  }
  return out;
}

/** The real folders of this build. */
export function defaultSources(): AppRouteSources {
  const lazy = byFolder(
    import.meta.glob<FolderModule>([
      '../auth/index.{ts,tsx}',
      '../setup/index.{ts,tsx}',
      '../account/index.{ts,tsx}',
      '../admin/index.{ts,tsx}',
      '../download/index.{ts,tsx}',
      '../rooms/index.{ts,tsx}',
      '../link/index.{ts,tsx}',
      '../desktop/index.{ts,tsx}',
    ]),
  );
  return ROOMS_IN_TESTS === undefined ? { lazy } : { lazy, eager: { rooms: ROOMS_IN_TESTS } };
}

function isComponent(v: unknown): v is ComponentType {
  // Function and class components, and React.memo/forwardRef objects.
  return typeof v === 'function' || (typeof v === 'object' && v !== null && '$$typeof' in v);
}

/** Renders the child route: what a layout route shows while its folder has no layout yet. */
function OutletOnly() {
  return <Outlet />;
}

/** Renders a component that a folder exports; nothing while the folder is not loaded, or has no such export. */
function Exported({ component: Component }: { component: unknown }) {
  return isComponent(Component) ? <Component /> : null;
}

/**
 * The app's routes for a target (default 'browser', which M1's callers and tests get). A page that only the other
 * target shows keeps its handle but has no `lazy`: its folder is never loaded here, and NotFound stands in for the
 * page, which its guard doesn't render anyway.
 */
export function createAppRoutes(
  sources: AppRouteSources = defaultSources(),
  target: RouteTarget = 'browser',
): RouteObject[] {
  /** The folders that are loaded so far, by name: the layout shows a part of one of them (Root, below). */
  const loaded = createStore<Partial<Record<PageFolder, FolderModule>>>(() => ({ ...sources.eager }));

  /** A page route's `handle` and its `lazy`; for a folder that is there already, its Component instead. */
  const page = (folder: PageFolder, name: string, fallback: ComponentType = PageUnavailable) => {
    const handle: RouteHandle = { folder, page: name };
    const pick = (mod: FolderModule | undefined): ComponentType => {
      const Component = mod?.[name];
      return isComponent(Component) ? Component : fallback;
    };
    const eager = sources.eager?.[folder];
    if (eager !== undefined) return { handle, Component: pick(eager) };
    return {
      handle,
      lazy: async () => {
        const mod = await sources.lazy[folder]?.();
        if (mod !== undefined) loaded.setState({ [folder]: mod });
        return { Component: pick(mod) };
      },
    };
  };
  /** A layout route: renders only its child until the folder has the layout. */
  const layout = (folder: PageFolder, name: string) => page(folder, name, OutletOnly);

  /** page() and layout() for the pages of one target: in the other target's table they load nothing. */
  const pagesOf = (where: RouteTarget): { page: typeof page; layout: typeof layout } => {
    if (where === target) return { page, layout };
    const elsewhere: typeof page = (folder, name) => ({ handle: { folder, page: name }, Component: NotFound });
    return { page: elsewhere, layout: elsewhere };
  };
  /** The browser's pages: in the desktop app RequireBrowser opens them in the system browser instead. */
  const browser = pagesOf('browser');
  /** The desktop app's pages: RequireDesktop shows NotFound in the browser. */
  const desktop = pagesOf('desktop');

  /**
   * The root route's layout, with rooms/'s InRoomBar above the pages from the moment rooms/ is loaded: the first
   * visit to `/` or to a room loads it, and only a room's page makes the runtime that the bar shows.
   */
  function Root() {
    const bar = useStore(loaded, (s) => s.rooms?.['InRoomBar']);
    return <RootLayout above={<Exported component={bar} />} />;
  }

  return [
    {
      id: 'root',
      Component: Root,
      ErrorBoundary: RouteErrorBoundary,
      HydrateFallback: PageSpinner,
      children: [
        {
          ErrorBoundary: RouteErrorBoundary,
          children: [
            // The browser's pages (docs/m2/03 §7.5, D7). In the desktop app RequireBrowser opens the page in the
            // system browser (/link with its ?code=) and renders "Opened in your browser"; in the browser it renders
            // the page.
            {
              Component: RequireBrowser,
              children: [
                // Public (05 §5 "public"): these read ['me'] without redirecting.
                { path: 'login', ...browser.page('auth', 'LoginPage') },
                { path: 'invite', ...browser.page('auth', 'InvitePage') },
                { path: 'signup', ...browser.page('auth', 'SignupPage') },
                { path: 'pending', ...browser.page('auth', 'PendingPage') },
                { path: 'reset', ...browser.page('auth', 'ResetPage') },
                // Public + the fragment token; the server answers 404 once an admin exists (05 §14).
                { path: 'setup', ...browser.page('setup', 'SetupPage') },
                // The server's download page (M2: the desktop installer, 06 §7.5).
                { path: 'download', ...browser.page('download', 'DownloadPage') },

                // Signed-in users, with a cookie session: the desktop's bearer can't do what these pages do.
                {
                  Component: RequireAuth,
                  children: [
                    { path: 'account', ...browser.page('account', 'AccountPage') },
                    { path: 'account/devices', ...browser.page('account', 'DevicesPage') },
                    { path: 'account/notifications', ...browser.page('account', 'NotificationsPage') },
                    // M2: the RFC 8628 approval page (/link?code=…, 02 §10.1). NotFound until link/ has it.
                    { path: 'link', ...browser.page('link', 'DeviceLinkPage', NotFound) },
                    {
                      path: 'admin',
                      children: [
                        // Wizard steps 2–3, re-enterable (05 §14).
                        {
                          path: 'welcome',
                          Component: RequireAdmin,
                          children: [{ index: true, ...browser.page('setup', 'WelcomePage') }],
                        },
                        // Admins, and members with me.permissions.createInvites (they see only their own invites).
                        {
                          path: 'invites',
                          Component: RequireInviter,
                          children: [
                            {
                              ...browser.layout('admin', 'AdminLayout'),
                              children: [{ index: true, ...browser.page('admin', 'InvitesPage') }],
                            },
                          ],
                        },
                        {
                          Component: RequireAdmin,
                          children: [
                            {
                              ...browser.layout('admin', 'AdminLayout'),
                              children: [
                                { index: true, ...browser.page('admin', 'DashboardPage') },
                                { path: 'users', ...browser.page('admin', 'UsersPage') },
                                // Nav badge from me.badges.pendingApprovals.
                                { path: 'approvals', ...browser.page('admin', 'ApprovalsPage') },
                                { path: 'rooms', ...browser.page('admin', 'RoomsPage') },
                                { path: 'settings', ...browser.page('admin', 'SettingsPage') },
                                // 03's audit log (GET /api/v1/admin/audit).
                                { path: 'audit', ...browser.page('admin', 'AuditPage') },
                                // Doctor report, connection test and bandwidth calculator.
                                { path: 'doctor', ...browser.page('admin', 'DoctorPage') },
                              ],
                            },
                          ],
                        },
                      ],
                    },
                  ],
                },
              ],
            },

            // The trust model and versions (W3 writes it with the auth pages). Public, in both.
            { path: 'about', ...page('auth', 'AboutPage') },

            // Signed-in users, in both. The desktop's RequireAuth decides by the app's AuthState, never /login.
            {
              Component: RequireAuth,
              children: [
                // → /r/<lastRoomId> if that room still exists, else /r/<defaultRoomId>.
                { index: true, ...page('rooms', 'RootRedirect') },
                // ?focus=<shareId> focuses that share (push links, 04 §14.3).
                { path: 'r/:roomId', ...page('rooms', 'RoomPage') },
              ],
            },

            // The desktop app's own pages (docs/m2/03 §7.5): NotFound in the browser.
            {
              path: 'desktop',
              Component: RequireDesktop,
              children: [
                // The first-run wizard (§8.2): ?add=<origin> for a new server, ?link=1 after Auth.AddServer.
                { path: 'setup', ...desktop.page('desktop', 'SetupPage') },
                // The active server signed this PC out (§8.3).
                { path: 'relink', ...desktop.page('desktop', 'RelinkPage') },
                // §14.4; anchors #servers, #sharing, #audio, #app, #updates, #about.
                { path: 'settings', ...desktop.page('desktop', 'SettingsPage') },
              ],
            },

            { path: '*', Component: NotFound },
          ],
        },
      ],
    },
  ];
}

/**
 * Asks for the page folders of a path's routes without waiting for them: what the router of `target` loads for that
 * path, one round trip earlier. Boot calls it with the first path before it asks for /api/v1/info (05 §4), so the
 * first page's chunk and /info travel together. A folder that is loaded already answers the router's own import() at
 * once; one that can't be loaded here is the router's to report when it gets to the route (RouteErrorBoundary).
 */
export function preloadRoute(
  pathname: string,
  sources: AppRouteSources = defaultSources(),
  target: RouteTarget = 'browser',
): void {
  const folders = new Set<PageFolder>();
  for (const { route } of matchRoutes(createAppRoutes(sources, target), pathname) ?? []) {
    const handle = route.handle as RouteHandle | undefined;
    // What the router would load: a route with `lazy` (not an eager folder, not a page of the other target).
    if (handle !== undefined && route.lazy !== undefined) folders.add(handle.folder);
  }
  for (const folder of folders) sources.lazy[folder]?.().catch(() => undefined);
}
