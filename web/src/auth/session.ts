// The two moments the identity in this tab's REST cache changes (05 §15.1): a form just created a session (login,
// register with an invite, setup, reset), or the user logged out (logout.ts). No React here, so logout.ts and the
// controllers that call it stay usable outside components.
//
// Not to be confused with app/session.ts, the BroadcastChannel that tells the user's other tabs about a logout.
import type { QueryClient, QueryKey } from '@tanstack/react-query';

import { clearMe, meQueryOptions } from '../app/me';
import type { Me } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';

/**
 * The server answered a sign-in with 2xx, but GET /api/v1/me right after says "signed out": the browser didn't keep
 * the session cookie (cookies blocked for this site, or an embedded browser that drops them). Shown as
 * auth.sessionNotKept.
 */
export class SessionNotKeptError extends Error {
  override readonly name = 'SessionNotKeptError';

  constructor() {
    super('auth: the session cookie was not kept');
  }
}

/** The first element of the query keys this folder makes itself (useLinkCheck.ts): ['auth', …]. */
export const AUTH_QUERY_ROOT = 'auth';

/**
 * What survives a change of identity: ['me'] exactly (it is set, not dropped), ['info'] (public), and the token
 * pages' own ['auth', …] link checks, which are about a link, not a user, and whose page is still open when its
 * form signs the user in. Everything else belongs to a user.
 */
function isShared(key: QueryKey): boolean {
  if (key[0] === queryKeys.me[0]) return key.length === 1;
  return key[0] === queryKeys.info[0] || key[0] === AUTH_QUERY_ROOT;
}

/** Drops every cached REST resource of the user this tab had before. Their observers are about to unmount. */
function forgetUserData(queryClient: QueryClient): void {
  queryClient.removeQueries({ predicate: (q) => !isShared(q.queryKey) });
}

/**
 * After a 2xx from login, register (201), setup/complete or reset/complete: the session cookie is set, so ['me'] is
 * loaded again before the page navigates (RequireAuth would bounce a cached "signed out" back to /login), and
 * whatever a previous user of this tab left in the cache is dropped.
 *
 * Resolves with the user, or undefined when GET /api/v1/me couldn't be reached (the guard then shows its Offline
 * screen with "Try now"). Rejects with SessionNotKeptError when the server still says "signed out".
 */
export async function startSession(queryClient: QueryClient): Promise<Me | undefined> {
  forgetUserData(queryClient);
  await queryClient.cancelQueries({ queryKey: queryKeys.me, exact: true });
  let me: Me | null;
  try {
    // One try: when it fails the page navigates anyway, and the guard asks again with its own retries.
    me = await queryClient.query({ ...meQueryOptions(), staleTime: 0, retry: false });
  } catch {
    return undefined;
  }
  if (me === null) throw new SessionNotKeptError();
  return me;
}

/**
 * The user logged out in this tab: ['me'] becomes null (guarded routes go to the login page, public pages stay) and
 * nothing user-specific stays cached. This is 05 §15.1's `queryClient.clear()` without its two side effects: a
 * cleared ['me'] would leave mounted guards showing the old user until they re-render, and a cleared ['info'] (public,
 * seeded at boot) would make the login page load it again.
 */
export function endSession(queryClient: QueryClient): void {
  clearMe(queryClient);
  forgetUserData(queryClient);
  queryClient.getMutationCache().clear();
}
