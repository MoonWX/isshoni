import { QueryClient } from '@tanstack/react-query';
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
    expect(queryKeysForTopic(topic)).toEqual(keys);
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

  it('matches a topic key by prefix: me also covers me/sessions and me/devices', async () => {
    const qc = seeded();
    await applyInvalidate(qc, { topics: ['me'] });
    expect(invalidated(qc)).toEqual(['["me","devices"]', '["me","sessions"]', '["me"]'].sort());
  });

  it('does nothing for an empty or unknown topic list', async () => {
    const qc = seeded();
    await applyInvalidate(qc, { topics: [] });
    await applyInvalidate(qc, { topics: ['nope' as types.Topic] });
    expect(invalidated(qc)).toEqual([]);
  });
});
