// The signed-in user for pages (05 §5, §6.2), on top of app/me.ts.
//
// useMe() is the no-redirect read that public pages use (/login, /invite, /signup, /pending, /reset, /setup,
// /download, /about): a 401 from GET /api/v1/me means "signed out" and resolves to null, and nothing redirects.
// Pages under RequireAuth are behind the guard (app/guards.tsx), which redirects on null, so there useMe().data is
// the user. What changes the cached user is in session.ts (a sign-in, a logout) and in the app shell (a 401 from any
// request, a logout in another tab).
import type { UseQueryResult } from '@tanstack/react-query';

import { useMeQuery } from '../app/me';
import type { Me } from '../protocol/api.gen';

/**
 * GET /api/v1/me: data is the user, null when signed out, undefined while loading or when the request failed for
 * another reason than a 401 (isError).
 */
export function useMe(): UseQueryResult<Me | null> {
  return useMeQuery();
}
