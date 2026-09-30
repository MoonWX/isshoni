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

/** Topic (01 §8.12) → the query keys it invalidates (05 §6.2). */
export const topicQueryKeys: Readonly<Record<Topic, readonly (readonly string[])[]>> = {
  [TopicRooms]: [queryKeys.rooms],
  [TopicMe]: [queryKeys.me],
  [TopicDevices]: [queryKeys.meSessions, queryKeys.meDevices],
  [TopicAdminUsers]: [queryKeys.adminUsers],
  [TopicAdminInvites]: [queryKeys.invites],
  // The approvals badge is in Me.badges.
  [TopicAdminApprovals]: [queryKeys.adminApprovals, queryKeys.me],
  // Settings change what /info reports (registration mode, server name, push).
  [TopicAdminSettings]: [queryKeys.adminSettings, queryKeys.info],
};

/** The query keys of one topic; none for a topic this build doesn't know (newer servers may add topics). */
export function queryKeysForTopic(topic: string): readonly (readonly string[])[] {
  return Object.hasOwn(topicQueryKeys, topic) ? topicQueryKeys[topic as Topic] : [];
}

/**
 * Invalidates every query key of the message's topics, each once. Active queries refetch; inactive ones refetch when
 * next used. Unknown topics are ignored.
 */
export async function applyInvalidate(queryClient: QueryClient, msg: Invalidate): Promise<void> {
  const seen = new Set<string>();
  const keys: (readonly string[])[] = [];
  for (const topic of msg.topics) {
    for (const key of queryKeysForTopic(topic)) {
      const id = JSON.stringify(key);
      if (!seen.has(id)) {
        seen.add(id);
        keys.push(key);
      }
    }
  }
  await Promise.all(keys.map((queryKey) => queryClient.invalidateQueries({ queryKey })));
}
