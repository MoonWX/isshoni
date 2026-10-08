// The signed-in user for pages (05 §5, §6.2), on top of app/me.ts.
//
// useMe() is the no-redirect read that public pages use (/invite, /signup, /pending, /reset, /setup, /download,
// /about): a 401 from GET /api/v1/me means "signed out" and resolves to null, and nothing redirects.
// Pages under RequireAuth are behind the guard (app/guards.tsx), which redirects on null, so there useMe().data is
// the user. What changes the cached user is in session.ts (a sign-in, a logout) and in the app shell (a 401 from any
// request, a logout in another tab).
//
// useConfirmedMe() is the login page's read: there "signed in" sends the user away from the page, so a user that is
// only cached is confirmed with the server first.
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import { meQueryOptions, useMeQuery } from '../app/me';
import type { Me } from '../protocol/api.gen';

/**
 * GET /api/v1/me: data is the user, null when signed out, undefined while loading or when the request failed for
 * another reason than a 401 (isError).
 */
export function useMe(): UseQueryResult<Me | null> {
  return useMeQuery();
}

/**
 * useMe() for a page that acts on "signed in" by itself. A user that is already cached when the page opens is asked
 * for again, however fresh the cache is, and counts only once the server has answered: `data` is the cached user
 * and `isFetching` is true until then. The cache can be behind the server: a navigation to /login is often made
 * because the session just ended (loginNotice.ts). A cached "signed out" is taken as it is, like useMe() does.
 */
export function useConfirmedMe(): UseQueryResult<Me | null> {
  return useQuery({
    ...meQueryOptions(),
    refetchOnMount: (query) => (query.state.data ? 'always' : true),
  });
}
