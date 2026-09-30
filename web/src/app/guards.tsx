// Route guards (05 §5), as layout routes that render <Outlet /> when access is granted.
// - RequireAuth loads ['me'] and sends signed-out users to /login?next=<path>.
// - RequireAdmin also needs me.user.role === "admin"; others get NotFound, so admin pages aren't advertised.
// - RequireInviter allows admins and users with me.permissions.createInvites (only /admin/invites), else NotFound.
// `next` is accepted only if it starts with "/" and not "//" (open-redirect guard); see safeNext.
import type { ReactNode } from 'react';
import { Navigate, Outlet, useLocation } from 'react-router';

import { RoleAdmin, type Me } from '../protocol/api.gen';
import { ApiError, isRetryableError } from '../protocol/rest';
import { PageSpinner } from '../ui/Spinner';
import { useMeQuery } from './me';
import { NotFound } from './NotFound';
import { Fatal } from './screens/Fatal';
import { Offline } from './screens/Offline';

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

/** Loads ['me'] and renders children(me) once it is known; spinner, error screen or login redirect otherwise. */
function WithMe({ children }: { children: (me: Me) => ReactNode }) {
  const location = useLocation();
  const me = useMeQuery();
  if (me.isPending) return <PageSpinner />;
  if (me.isError) {
    // Not a 401 (that resolves to null). Unreachable or failing after the query's own retries: offer to try again.
    if (isRetryableError(me.error)) {
      return <Offline onRetry={() => void me.refetch()} retrying={me.isFetching} />;
    }
    return <Fatal code={me.error instanceof ApiError ? me.error.code : undefined} />;
  }
  if (me.data === null) return <Navigate to={loginPath(location)} replace />;
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
