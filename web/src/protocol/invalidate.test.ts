import { QueryClient, QueryObserver } from '@tanstack/react-query';
import { describe, expect, it } from 'vitest';

import { applyInvalidate, queryKeysForTopic, topicQueryKeys } from './invalidate';
import { queryKeys } from './queryKeys';
import * as types from './types.gen';

describe('invalidate topic → query keys (05 §6.2)', () => {
  it.each([
    ['rooms', [['rooms']]],
    ['me', [['me']]],
    [
      'devices',
      [
        ['me', 'sessions'],
        ['me', 'devices'],
      ],
    ],
    ['admin.users', [['admin', 'users']]],
    ['admin.invites', [['invites']]],
    ['admin.approvals', [['admin', 'approvals'], ['me']]],
    ['admin.settings', [['admin', 'settings'], ['info']]],
  ])('%s → %j', (topic, keys) => {
    expect(queryKeysForTopic(topic).map((k) => k.queryKey)).toEqual(keys);
  });

  it('matches ["me"] exactly and every other key by prefix', () => {
    const exact = Object.values(topicQueryKeys)
      .flat()
      .filter((k) => k.exact === true)
      .map((k) => k.queryKey);
    expect(new Set(exact.map((k) => JSON.stringify(k)))).toEqual(new Set([JSON.stringify(queryKeys.me)]));
  });

  it('covers every Topic constant of 01 §8.12 (types.gen.ts)', () => {
    const topics = Object.entries(types)
      .filter(([name, v]) => /^Topic[A-Z]/.test(name) && typeof v === 'string')
      .map(([, v]) => v as string)
      .sort();
    expect(Object.keys(topicQueryKeys).sort()).toEqual(topics);
  });

  it('ignores topics this build does not know', () => {
    expect(queryKeysForTopic('admin.future')).toEqual([]);
    expect(queryKeysForTopic('toString')).toEqual([]);
  });
});

describe('applyInvalidate', () => {
  function seeded(): QueryClient {
    const qc = new QueryClient();
    for (const key of Object.values(queryKeys)) qc.setQueryData(key, { v: 1 });
    qc.setQueryData([...queryKeys.invites, { state: 'all' }], { v: 1 });
    return qc;
  }

  const invalidated = (qc: QueryClient): string[] =>
    qc
      .getQueryCache()
      .getAll()
      .filter((q) => q.state.isInvalidated)
      .map((q) => JSON.stringify(q.queryKey))
      .sort();

  it('invalidates exactly the keys of the topics, including parameterized forms', async () => {
    const qc = seeded();
    await applyInvalidate(qc, { topics: ['admin.invites', 'devices'] });
    expect(invalidated(qc)).toEqual(
      ['["invites"]', '["invites",{"state":"all"}]', '["me","devices"]', '["me","sessions"]'].sort(),
    );
  });

  it('me and the approvals badge invalidate ["me"] only, not me/sessions or me/devices (those are devices)', async () => {
    let qc = seeded();
    await applyInvalidate(qc, { topics: ['me'] });
    expect(invalidated(qc)).toEqual(['["me"]']);
    qc = seeded();
    await applyInvalidate(qc, { topics: ['admin.approvals'] });
    expect(invalidated(qc)).toEqual(['["admin","approvals"]', '["me"]'].sort());
    qc = seeded();
    await applyInvalidate(qc, { topics: ['me', 'devices'] });
    expect(invalidated(qc)).toEqual(['["me","devices"]', '["me","sessions"]', '["me"]'].sort());
  });

  it('refetches an active me/sessions query only for devices', async () => {
    const qc = seeded();
    let fetches = 0;
    const observer = new QueryObserver(qc, {
      queryKey: queryKeys.meSessions,
      queryFn: () => {
        fetches++;
        return { v: 2 };
      },
      staleTime: Infinity,
    });
    const unsubscribe = observer.subscribe(() => undefined);
    await applyInvalidate(qc, { topics: ['me', 'admin.approvals'] });
    expect(fetches).toBe(0);
    await applyInvalidate(qc, { topics: ['devices'] });
    expect(fetches).toBe(1);
    unsubscribe();
  });

  it('does nothing for an empty or unknown topic list', async () => {
    const qc = seeded();
    await applyInvalidate(qc, { topics: [] });
    await applyInvalidate(qc, { topics: ['nope' as types.Topic] });
    expect(invalidated(qc)).toEqual([]);
  });
});
