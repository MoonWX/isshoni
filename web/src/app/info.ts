// The server's public info: GET /api/v1/info under the ['info'] query key (05 §4, §6.2). Boot fetches it before the
// first render and seeds the query with seedInfo(), so it is there from the first page on; pages read it with
// useInfo() (registration mode and account rules for signup, the server name, features), never with getQueryData.
// The query has a queryFn, so the `admin.settings` invalidation (protocol/invalidate.ts) and staleness refetch it,
// and it is kept for the page's life (gcTime Infinity) even while no page reads it.
import { queryOptions, useQuery, type QueryClient, type UseQueryResult } from '@tanstack/react-query';

import type { Info } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { api } from '../protocol/rest';

export function fetchInfo(signal?: AbortSignal): Promise<Info> {
  return api<Info>('GET', '/api/v1/info', undefined, signal ? { signal } : {});
}

export function infoQueryOptions() {
  return queryOptions({
    queryKey: queryKeys.info,
    queryFn: ({ signal }) => fetchInfo(signal),
    gcTime: Infinity,
  });
}

/** The server's info; boot seeded it, so data is there on the first render. */
export function useInfo(): UseQueryResult<Info> {
  return useQuery(infoQueryOptions());
}

/** Boot step 5 (05 §4): the /info that boot loaded becomes the ['info'] query, kept for the page's life. */
export function seedInfo(queryClient: QueryClient, info: Info): void {
  const { queryKey, gcTime } = infoQueryOptions();
  // Defaults, so the cache entry that setQueryData creates already has them before any page observes it.
  queryClient.setQueryDefaults(queryKey, { queryFn: ({ signal }) => fetchInfo(signal), gcTime });
  queryClient.setQueryData(queryKey, info);
}
