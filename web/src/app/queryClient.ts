// The TanStack Query client (05 §6.2): defaults, retries and the global 401 rule.
// - staleTime 30 s; refetchOnWindowFocus off (admin lists turn it on per query).
// - Queries retry network errors, 5xx, server_busy and rate_limited up to 3 times (after retryAfter when the server
//   gives one), nothing else. Mutations never retry: they are user actions, and most aren't idempotent.
// - A 401 unauthenticated (M2: invalid_token) from any query or mutation clears ['me'] (app/me.ts), which sends
//   guarded routes to /login?next=…; public pages keep showing.
import { MutationCache, QueryCache, QueryClient } from '@tanstack/react-query';

import { ApiError, isRetryableError, isSignedOutError } from '../protocol/rest';
import { clearMe } from './me';

export const STALE_TIME_MS = 30_000;
export const MAX_RETRIES = 3;

/** TanStack's retry predicate: failureCount failures so far. */
export function shouldRetry(failureCount: number, error: unknown): boolean {
  return failureCount < MAX_RETRIES && isRetryableError(error);
}

/** The wait before retry number attempt+1: the server's retryAfter when it gave one, else 1 s, 2 s, 4 s … (≤ 8 s). */
export function retryDelayMs(attempt: number, error: unknown): number {
  if (error instanceof ApiError && error.retryAfterSec !== undefined) return error.retryAfterSec * 1000;
  return Math.min(1000 * 2 ** attempt, 8000);
}

export function createQueryClient(): QueryClient {
  const onError = (error: unknown): void => {
    if (isSignedOutError(error)) clearMe(queryClient);
  };
  const queryClient: QueryClient = new QueryClient({
    queryCache: new QueryCache({ onError }),
    mutationCache: new MutationCache({ onError }),
    defaultOptions: {
      queries: {
        staleTime: STALE_TIME_MS,
        retry: shouldRetry,
        retryDelay: retryDelayMs,
        refetchOnWindowFocus: false,
      },
      mutations: { retry: false },
    },
  });
  return queryClient;
}
