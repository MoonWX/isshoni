// 01's `invalidate` message → TanStack Query invalidation (05 §6.2, 01 §8.12). The server names groups of REST
// resources (topics); this map says which query keys each one covers. rooms/ (S34) subscribes the SignalClient's
// `invalidate` messages to applyInvalidate.
import type { QueryClient } from '@tanstack/react-query';

import { queryKeys } from './queryKeys';
import {
  TopicAdminApprovals,
  TopicAdminInvites,
  TopicAdminSettings,
  TopicAdminUsers,
  TopicDevices,
  TopicMe,
  TopicRooms,
  type Invalidate,
  type Topic,
} from './types.gen';

/**
 * One query key a topic invalidates. TanStack matches keys by prefix, which also reaches a base key's parameterized
 * forms (['invites', {state: 'all'}]); `exact` limits it to the key itself, for a key that other resources extend:
 * ['me'] must not reach ['me','sessions'] and ['me','devices'], which only `devices` covers (05 §6.2).
 */
export interface TopicQueryKey {
  readonly queryKey: readonly string[];
  readonly exact?: boolean;
}

const prefix = (queryKey: readonly string[]): TopicQueryKey => ({ queryKey });
const me: TopicQueryKey = { queryKey: queryKeys.me, exact: true };

/** Topic (01 §8.12) → the query keys it invalidates (05 §6.2). */
export const topicQueryKeys: Readonly<Record<Topic, readonly TopicQueryKey[]>> = {
  [TopicRooms]: [prefix(queryKeys.rooms)],
  [TopicMe]: [me],
  [TopicDevices]: [prefix(queryKeys.meSessions), prefix(queryKeys.meDevices)],
  [TopicAdminUsers]: [prefix(queryKeys.adminUsers)],
  [TopicAdminInvites]: [prefix(queryKeys.invites)],
  // The approvals badge is in Me.badges.
  [TopicAdminApprovals]: [prefix(queryKeys.adminApprovals), me],
  // Settings change what /info reports (registration mode, server name, push).
  [TopicAdminSettings]: [prefix(queryKeys.adminSettings), prefix(queryKeys.info)],
};

/** The query keys of one topic; none for a topic this build doesn't know (newer servers may add topics). */
export function queryKeysForTopic(topic: string): readonly TopicQueryKey[] {
  return Object.hasOwn(topicQueryKeys, topic) ? topicQueryKeys[topic as Topic] : [];
}

/**
 * Invalidates every query key of the message's topics, each once (a prefix match wins over an exact one for the same
 * key). Active queries refetch; inactive ones refetch when next used. Unknown topics are ignored.
 */
export async function applyInvalidate(queryClient: QueryClient, msg: Invalidate): Promise<void> {
  const keys = new Map<string, TopicQueryKey>();
  for (const topic of msg.topics) {
    for (const key of queryKeysForTopic(topic)) {
      const id = JSON.stringify(key.queryKey);
      const seen = keys.get(id);
      keys.set(id, seen === undefined || seen.exact === true ? key : seen);
    }
  }
  await Promise.all(
    [...keys.values()].map(({ queryKey, exact = false }) => queryClient.invalidateQueries({ queryKey, exact })),
  );
}
