// The "is this link still good?" request of the token pages (03 §7.8–§7.10): POST /api/v1/auth/<kind>/check {token},
// sent when the page opens, so a dead link says so before anyone fills in a form.
//   invite → 200 InviteInfo            | invite_invalid, invite_expired, invite_used_up, invite_revoked,
//                                        registration_closed
//   setup  → 204                       | setup_token_invalid, setup_unavailable
//   reset  → 200 ResetCheckResponse    | reset_token_invalid
import { useQuery, type UseQueryResult } from '@tanstack/react-query';

import type { InviteInfo, ResetCheckResponse, TokenRequest } from '../protocol/api.gen';
import { api } from '../protocol/rest';
import type { FragmentTokenKind } from './fragmentToken';
import { AUTH_QUERY_ROOT } from './session';

/** What each check answers. */
export interface LinkCheckResult {
  invite: InviteInfo;
  /** 204: the link works. */
  setup: null;
  reset: ResetCheckResponse;
}

/**
 * Checks the page's token once. Disabled without a token (the page then says the link is incomplete) or when
 * `enabled` is false.
 *
 * - No automatic retries: every attempt spends the auth-ip budget (03 §7.3), and a rate_limited answer would keep
 *   the page waiting silently. The page offers "Try again" (refetch) for failures that may pass.
 * - The result isn't kept after the page closes (gcTime 0): the cache key holds the token, in memory only (05 §20).
 */
export function useLinkCheck<K extends FragmentTokenKind>(
  kind: K,
  token: string | null,
  enabled = true,
): UseQueryResult<LinkCheckResult[K]> {
  return useQuery({
    queryKey: [AUTH_QUERY_ROOT, 'link-check', kind, token],
    queryFn: async (): Promise<LinkCheckResult[K]> => {
      const body: TokenRequest = { token: token ?? '' };
      const result = await api<LinkCheckResult[K] | undefined>('POST', `/api/v1/auth/${kind}/check`, body);
      // 204 has no body; a query's data can't be undefined.
      return (result ?? null) as LinkCheckResult[K];
    },
    enabled: enabled && token !== null,
    retry: false,
    staleTime: Infinity,
    gcTime: 0,
    refetchOnReconnect: false,
  });
}
