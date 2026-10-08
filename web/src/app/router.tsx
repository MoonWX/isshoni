// Every route of 05 §5, declared once here (README "Shared files": S27 owns this file; page slices fill only their
// folder). React Router data router: every page except the room is a lazy chunk.
//
// Page folders. Each route loads one page component, by name, from its folder's entry module `<folder>/index.ts`
// (or index.tsx), which exports the folder's pages:
//   auth/      LoginPage InvitePage SignupPage PendingPage ResetPage AboutPage      (S33; lazy chunk)
//   setup/     SetupPage WelcomePage                                              (S33, S87; lazy chunk)
//   account/   AccountPage DevicesPage NotificationsPage                          (S48, S77; lazy chunk)
//   admin/     AdminLayout DashboardPage UsersPage ApprovalsPage InvitesPage       (S49, S91; lazy chunk)
//              RoomsPage SettingsPage AuditPage DoctorPage
//   download/  DownloadPage                                                       (later; lazy chunk)
//   rooms/     RootRedirect RoomPage                                              (S34, S45; main chunk)
// The folders are found with import.meta.glob, so a folder that doesn't exist yet is simply absent: its routes
// render PageUnavailable (a missing AdminLayout renders just its child page) until the slice adds the export, and
// no route here changes. A lazy folder is one chunk, loaded on the first visit to one of its routes; rooms/ is
// bundled eagerly (the room is the main page, 05 §5).
//
// No route segment contains a dot: 04 §9.5 serves dotted paths as files.
import type { ComponentType } from 'react';
import { Outlet, type RouteObject } from 'react-router';

import { RouteErrorBoundary } from './ErrorBoundary';
import { RequireAdmin, RequireAuth, RequireInviter } from './guards';
import { RootLayout } from './layouts/RootLayout';
import { NotFound, PageUnavailable } from './NotFound';
import { PageSpinner } from '../ui/Spinner';

/** A page folder's entry module: page components by export name. */
export type FolderModule = Readonly<Record<string, unknown>>;

export type LazyFolder = 'auth' | 'setup' | 'account' | 'admin' | 'download';
export type EagerFolder = 'rooms';
export type PageFolder = LazyFolder | EagerFolder;

/** What each route loads, for tests and debugging: route.handle. */
export interface RouteHandle {
  readonly folder: PageFolder;
  readonly page: string;
}

export interface AppRouteSources {
  /** Loaders of the lazy folders; a missing folder renders PageUnavailable. */
  lazy: Partial<Record<LazyFolder, () => Promise<FolderModule>>>;
  /** Modules of the eager folders. */
  eager: Partial<Record<EagerFolder, FolderModule>>;
}

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
    ]),
  );
  const eager = byFolder(import.meta.glob<FolderModule>('../rooms/index.{ts,tsx}', { eager: true }));
  return { lazy, eager };
}

function isComponent(v: unknown): v is ComponentType {
  // Function and class components, and React.memo/forwardRef objects.
  return typeof v === 'function' || (typeof v === 'object' && v !== null && '$$typeof' in v);
}

/** Renders the child route: what a layout route shows while its folder has no layout yet. */
function OutletOnly() {
  return <Outlet />;
}

export function createAppRoutes(sources: AppRouteSources = defaultSources()): RouteObject[] {
  /** A lazy page route's `lazy` and `handle`. */
  const page = (folder: LazyFolder, name: string, fallback: ComponentType = PageUnavailable) => ({
    handle: { folder, page: name } satisfies RouteHandle,
    lazy: async () => {
      const load = sources.lazy[folder];
      const mod = load ? await load() : undefined;
      const Component = mod?.[name];
      return { Component: isComponent(Component) ? Component : fallback };
    },
  });
  /** A lazy layout route: renders only its child until the folder has the layout. */
  const layout = (folder: LazyFolder, name: string) => page(folder, name, OutletOnly);
  /** An eager page route's Component and handle. */
  const eager = (folder: EagerFolder, name: string) => {
    const Component = sources.eager[folder]?.[name];
    return {
      handle: { folder, page: name } satisfies RouteHandle,
      Component: isComponent(Component) ? Component : PageUnavailable,
    };
  };

  return [
    {
      id: 'root',
      Component: RootLayout,
      ErrorBoundary: RouteErrorBoundary,
      HydrateFallback: PageSpinner,
      children: [
        {
          ErrorBoundary: RouteErrorBoundary,
          children: [
            // Public (05 §5 "public"): these read ['me'] without redirecting.
            { path: 'login', ...page('auth', 'LoginPage') },
            { path: 'invite', ...page('auth', 'InvitePage') },
            { path: 'signup', ...page('auth', 'SignupPage') },
            { path: 'pending', ...page('auth', 'PendingPage') },
            { path: 'reset', ...page('auth', 'ResetPage') },
            // The trust model and versions (W3 writes it with the auth pages).
            { path: 'about', ...page('auth', 'AboutPage') },
            // Public + the fragment token; the server answers 404 once an admin exists (05 §14).
            { path: 'setup', ...page('setup', 'SetupPage') },
            // M1 placeholder; per-OS installers are later (M2).
            { path: 'download', ...page('download', 'DownloadPage') },

            // Signed-in users.
            {
              Component: RequireAuth,
              children: [
                // → /r/<lastRoomId> if that room still exists, else /r/<defaultRoomId>.
                { index: true, ...eager('rooms', 'RootRedirect') },
                // ?focus=<shareId> focuses that share (push links, 04 §14.3).
                { path: 'r/:roomId', ...eager('rooms', 'RoomPage') },
                { path: 'account', ...page('account', 'AccountPage') },
                { path: 'account/devices', ...page('account', 'DevicesPage') },
                { path: 'account/notifications', ...page('account', 'NotificationsPage') },
                // later (M2): DeviceLinkPage, the RFC 8628 approval page (/link?code=…). Reserved: NotFound in M1.
                { path: 'link', Component: NotFound },
                {
                  path: 'admin',
                  children: [
                    // Wizard steps 2–3, re-enterable (05 §14).
                    {
                      path: 'welcome',
                      Component: RequireAdmin,
                      children: [{ index: true, ...page('setup', 'WelcomePage') }],
                    },
                    // Admins, and members with me.permissions.createInvites (they see only their own invites).
                    {
                      path: 'invites',
                      Component: RequireInviter,
                      children: [
                        {
                          ...layout('admin', 'AdminLayout'),
                          children: [{ index: true, ...page('admin', 'InvitesPage') }],
                        },
                      ],
                    },
                    {
                      Component: RequireAdmin,
                      children: [
                        {
                          ...layout('admin', 'AdminLayout'),
                          children: [
                            { index: true, ...page('admin', 'DashboardPage') },
                            { path: 'users', ...page('admin', 'UsersPage') },
                            // Nav badge from me.badges.pendingApprovals.
                            { path: 'approvals', ...page('admin', 'ApprovalsPage') },
                            { path: 'rooms', ...page('admin', 'RoomsPage') },
                            { path: 'settings', ...page('admin', 'SettingsPage') },
                            // 03's audit log (GET /api/v1/admin/audit).
                            { path: 'audit', ...page('admin', 'AuditPage') },
                            // Doctor report, connection test and bandwidth calculator.
                            { path: 'doctor', ...page('admin', 'DoctorPage') },
                          ],
                        },
                      ],
                    },
                  ],
                },
              ],
            },

            { path: '*', Component: NotFound },
          ],
        },
      ],
    },
  ];
}
