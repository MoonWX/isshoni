// The signed-in user: GET /api/v1/me under the ['me'] query key (05 §5, §6.2). The query resolves to null when the
// server says the session is gone (401 unauthenticated; M2: invalid_token), so "signed out" is data, not an error:
// public pages read it without redirecting, and RequireAuth redirects on null. Clearing ['me'] to null (a 401 from
// any other query, or a logout in another tab) sends guarded routes to the login page.
//
// auth/useMe.ts (S33) builds on meQueryOptions; the guards in guards.tsx use useMeQuery directly.
//
// In the desktop app (M2) ['me'] is also null when Go refuses this page a token because the active server is
// incompatible (bridgeCodes.ts): the request was never sent, and this PC can't be signed in to that server until
// the app or the server is updated. RequireAuth then follows the AuthState to the link step, which shows why
// (docs/m2/03 §7.5, §8.2, §16.3), instead of showing the Offline screen for good.
import { queryOptions, useQuery, type QueryClient, type UseQueryResult } from '@tanstack/react-query';

import type { Me } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { api, isSignedOutError } from '../protocol/rest';
import { isIncompatibleServer } from './bridgeCodes';

export async function fetchMe(signal?: AbortSignal): Promise<Me | null> {
  try {
    return await api<Me>('GET', '/api/v1/me', undefined, signal ? { signal } : {});
  } catch (err) {
    if (isSignedOutError(err) || isIncompatibleServer(err)) return null;
    throw err;
  }
}

export function meQueryOptions() {
  return queryOptions({
    queryKey: queryKeys.me,
    queryFn: ({ signal }) => fetchMe(signal),
  });
}

export function useMeQuery(): UseQueryResult<Me | null> {
  return useQuery(meQueryOptions());
}

/** Marks the user as signed out in this tab: ['me'] becomes null, and guarded routes go to the login page. */
export function clearMe(queryClient: QueryClient): void {
  void queryClient.cancelQueries({ queryKey: queryKeys.me, exact: true });
  queryClient.setQueryData<Me | null>(queryKeys.me, null);
}
