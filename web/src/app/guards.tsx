// Route guards (05 §5; M2: docs/m2/03 §7.5), as layout routes that render <Outlet /> when access is granted.
// - RequireAuth loads ['me'] and sends signed-out users to /login?next=<path>. In the desktop app (M2) there is no
//   login page: it decides by the app's AuthState (DesktopSignedOut, below).
// - RequireAdmin also needs me.user.role === "admin"; others get NotFound, so admin pages aren't advertised.
// - RequireInviter allows admins and users with me.permissions.createInvites (only /admin/invites), else NotFound.
// - RequireDesktop (M2): the desktop app's own pages; NotFound in the browser.
// - RequireBrowser (M2): the pages that need the browser's cookie session (the auth pages, /setup, /account…,
//   /admin/*, /download and /link). In the desktop app it opens the page in the system browser, once per visit, and
//   says so (D7); in the browser it renders the page.
// `next` is accepted only if it starts with "/" and not "//" (open-redirect guard); see safeNext.
import { ExternalLink } from 'lucide-react';
import { useEffect, useRef, useState, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import { Navigate, Outlet, useLocation, useNavigate } from 'react-router';

import type { AuthNotice, AuthState } from '../platform/desktop/bridge';
import type { DesktopFeatures } from '../platform/types';
import { RoleAdmin, type Me } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { ApiError, isRetryableError } from '../protocol/rest';
import { Button } from '../ui/Button';
import { PageSpinner } from '../ui/Spinner';
import { useApp, usePlatform } from './context';
import { useMeQuery } from './me';
import { NotFound } from './NotFound';
import { Fatal } from './screens/Fatal';
import { Offline } from './screens/Offline';
import { ScreenFrame } from './screens/ScreenFrame';

/** The desktop app's pages that the guards and boot send to (docs/m2/03 §7.5, §8). */
export const DesktopPaths = {
  /** The first-run wizard; for an active server that isn't linked, its link step. */
  setup: '/desktop/setup',
  /** The active server signed this PC out. */
  relink: '/desktop/relink',
  /** Settings → Servers: "Switch server" of the Offline screen. */
  servers: '/desktop/settings#servers',
} as const;

/**
 * The path to return to after login, or "/" when the value is missing or unsafe: it must be a same-origin path,
 * so it starts with "/" and not "//" or "/\" (browsers read both as another host), and has no control characters.
 */
export function safeNext(raw: string | null | undefined): string {
  if (!raw?.startsWith('/')) return '/';
  if (raw.startsWith('//') || raw.startsWith('/\\')) return '/';
  // eslint-disable-next-line no-control-regex -- rejecting control characters is the point
  if (/[\u0000-\u001f\u007f]/.test(raw)) return '/';
  return raw;
}

/** /login?next=<the current path and query>. */
export function loginPath(location: { pathname: string; search: string }): string {
  const here = location.pathname + location.search;
  return here === '/' ? '/login' : `/login?next=${encodeURIComponent(here)}`;
}

export function isAdmin(me: Me): boolean {
  return me.user.role === RoleAdmin;
}

export function canInvite(me: Me): boolean {
  return isAdmin(me) || me.permissions.createInvites;
}

/** The desktop app's features, or null in the browser (M1's test platforms leave the member out). */
function useDesktop(): DesktopFeatures | null {
  return usePlatform().desktop ?? null;
}

/**
 * The notices of a refresh that the server refused with invalid_grant (02 §11.3, §11.4): the active server signed
 * this PC out, and the relink page says why (docs/m2/03 §8.3). The link step's own outcomes (denied, code_expired)
 * belong to the wizard, and save_failed comes with linked.
 */
const RELINK_NOTICES: ReadonlySet<AuthNotice> = new Set<AuthNotice>([
  'signed_out',
  'refresh_reused',
  'account_disabled',
]);

/**
 * Where the desktop app goes when ['me'] is null (docs/m2/03 §7.5), by the active server's AuthState:
 * - unlinked with a notice of a refused refresh → /desktop/relink;
 * - unlinked otherwise, linking, incompatible → /desktop/setup, the link step for the active server (it shows the
 *   link's progress, and incompatible with Try again);
 * - linked or offline → null: a 401 that a refresh cures. RequireAuth refetches ['me'] after the next auth.state.
 * Never /login: the desktop app has no cookie session.
 */
export function desktopSignedOutPath(state: Pick<AuthState, 'status' | 'notice'>): string | null {
  switch (state.status) {
    case 'unlinked':
      return RELINK_NOTICES.has(state.notice) ? DesktopPaths.relink : DesktopPaths.setup;
    case 'linking':
    case 'incompatible':
      return DesktopPaths.setup;
    case 'linked':
    case 'offline':
      return null;
  }
}

/**
 * The code of a bridge call's rejection. The transports reject with a BridgeError (platform/desktop/errors.ts); its
 * code is read by shape, because importing errors.ts here would split it into one more chunk of the first page load.
 */
function rejectionCode(err: unknown): string {
  const code = (err as { code?: unknown } | null | undefined)?.code;
  return typeof code === 'string' && code !== '' ? code : 'unknown';
}

/**
 * RequireAuth's signed-out branch in the desktop app: Auth.State() and the isshoni:auth.state events of this page's
 * server decide (desktopSignedOutPath). A state of another origin is ignored: the window is about to reload for the
 * new active server (docs/m2/02 §11.8, 03 §8.2).
 */
function DesktopSignedOut({ features }: { features: DesktopFeatures }) {
  const { platform, queryClient } = useApp();
  const origin = platform.serverOrigin;
  const [state, setState] = useState<AuthState | null>(null);
  const [failed, setFailed] = useState<string | null>(null);
  useEffect(() => {
    let live = true;
    const off = features.bridge.on('auth.state', (s) => {
      if (!live || s.origin !== origin) return;
      setState(s);
      // linked or offline: the server may take this PC's token again (Go refreshed it); ask for ['me'] once more.
      if (desktopSignedOutPath(s) === null) {
        void queryClient.refetchQueries({ queryKey: queryKeys.me, exact: true });
      }
    });
    features.bridge.auth.state().then(
      (s) => {
        // An event may have come first: it is the newer state.
        if (live && s.origin === origin) setState((prev) => prev ?? s);
      },
      (err: unknown) => {
        if (live) setFailed(rejectionCode(err));
      },
    );
    return () => {
      live = false;
      off();
    };
  }, [features, origin, queryClient]);
  if (failed !== null) return <Fatal code={failed} />;
  const to = state === null ? null : desktopSignedOutPath(state);
  if (to === null) return <PageSpinner />;
  return <Navigate to={to} replace />;
}

/** Loads ['me'] and renders children(me) once it is known; spinner, error screen or login redirect otherwise. */
function WithMe({ children }: { children: (me: Me) => ReactNode }) {
  const desktop = useDesktop();
  // The desktop app before its first server: there is nobody to be signed in to, and the wizard adds a server.
  if (desktop?.server === null) return <Navigate to={DesktopPaths.setup} replace />;
  return <WithMeQuery desktop={desktop}>{children}</WithMeQuery>;
}

function WithMeQuery({ desktop, children }: { desktop: DesktopFeatures | null; children: (me: Me) => ReactNode }) {
  const location = useLocation();
  const navigate = useNavigate();
  const me = useMeQuery();
  if (me.isPending) return <PageSpinner />;
  if (me.isError) {
    // Not a 401 (that resolves to null). Unreachable or failing after the query's own retries: offer to try again.
    if (isRetryableError(me.error)) {
      return (
        <Offline
          onRetry={() => void me.refetch()}
          retrying={me.isFetching}
          // The desktop app can go to another of its servers (docs/m2/03 §7.2).
          {...(desktop !== null ? { onSwitchServer: () => void navigate(DesktopPaths.servers) } : {})}
        />
      );
    }
    return <Fatal code={me.error instanceof ApiError ? me.error.code : undefined} />;
  }
  if (me.data === null) {
    return desktop !== null ? <DesktopSignedOut features={desktop} /> : <Navigate to={loginPath(location)} replace />;
  }
  return children(me.data);
}

/** Signed-in users only. */
export function RequireAuth() {
  return <WithMe>{() => <Outlet />}</WithMe>;
}

/** Admins only; everyone else sees NotFound. */
export function RequireAdmin() {
  return <WithMe>{(me) => (isAdmin(me) ? <Outlet /> : <NotFound />)}</WithMe>;
}

/** Admins and members who may create invites (me.permissions.createInvites); everyone else sees NotFound. */
export function RequireInviter() {
  return <WithMe>{(me) => (canInvite(me) ? <Outlet /> : <NotFound />)}</WithMe>;
}

/** The desktop app's pages (docs/m2/03 §7.5): NotFound in the browser. */
export function RequireDesktop() {
  return useDesktop() !== null ? <Outlet /> : <NotFound />;
}

/**
 * The pages that need the browser (docs/m2/03 §7.5, D7; /link: 02 §10.1, approval needs a cookie session and a
 * bearer gets 403 on /device/lookup). In the browser: the page. In the desktop app: openExternal(serverOrigin + path +
 * search) once per visit (so /link keeps its ?code=), and "Opened in your browser" with a Back button.
 */
export function RequireBrowser() {
  return useDesktop() !== null ? <OpenedInBrowser /> : <Outlet />;
}

function OpenedInBrowser() {
  const { t } = useTranslation();
  const platform = usePlatform();
  const location = useLocation();
  const navigate = useNavigate();
  const origin = platform.serverOrigin;
  // No server yet (boot keeps such an app on /desktop/setup): nothing to open.
  const url = origin === '' ? null : origin + location.pathname + location.search;
  /** The visit (location key) and URL opened last: once per visit, also when StrictMode runs the effect twice. */
  const opened = useRef<string | null>(null);
  useEffect(() => {
    if (url === null) return;
    const visit = `${location.key} ${url}`;
    if (opened.current === visit) return;
    opened.current = visit;
    platform.openExternal(url);
  }, [platform, url, location.key]);
  if (url === null) return <NotFound />;
  const back = (): void => {
    // The page before this one in the app, or the app's start when the app opened here.
    if (location.key !== 'default') void navigate(-1);
    else void navigate('/', { replace: true });
  };
  return (
    <ScreenFrame
      icon={ExternalLink}
      title={t('common.openedInBrowser.title')}
      actions={
        <Button variant="primary" onClick={back}>
          {t('common.back')}
        </Button>
      }
    >
      <p>{t('common.openedInBrowser.body')}</p>
    </ScreenFrame>
  );
}
