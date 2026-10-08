import { describe, expect, it } from 'vitest';

import type { ParticipantInfo, RoomState } from '../protocol/types.gen';
import { shareInfo } from './testing/publish';
import { shareWatchers } from './watchers';

const person = (userId: string, name: string): ParticipantInfo => ({
  userId,
  name,
  status: 'present',
  joinedAt: '2026-10-12T19:00:00.000Z',
  connections: [],
});

const state: Pick<RoomState, 'shares' | 'participants'> = {
  participants: [person('u_alex', 'Alex'), person('u_bea', 'Bea'), person('u_cy', 'Cy')],
  shares: [
    shareInfo('s_1', {
      watchers: [
        { userId: 'u_cy', video: 'high', audio: 'on' },
        { userId: 'u_bea', video: 'low', audio: 'off' },
        { userId: 'u_gone', video: 'low', audio: 'off' },
      ],
    }),
    shareInfo('s_2'),
  ],
};

describe('shareWatchers (05 §13.7)', () => {
  it('names who watches a share, in the snapshot’s order', () => {
    expect(shareWatchers(state, 's_1')).toEqual([
      { userId: 'u_cy', name: 'Cy' },
      { userId: 'u_bea', name: 'Bea' },
      // Not among the participants of this snapshot: the panel says "Someone".
      { userId: 'u_gone', name: '' },
    ]);
  });

  it('is empty for a share nobody watches, an unknown share, and without a snapshot or a share', () => {
    expect(shareWatchers(state, 's_2')).toEqual([]);
    expect(shareWatchers(state, 's_x')).toEqual([]);
    expect(shareWatchers(null, 's_1')).toEqual([]);
    expect(shareWatchers(state, null)).toEqual([]);
    expect(shareWatchers(undefined, undefined)).toEqual([]);
  });
});
