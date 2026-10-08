import { describe, expect, it } from 'vitest';

import type { ShareInfo, ShareStatus } from '../protocol/types.gen';
import { findShareElsewhere } from './elsewhere';

function share(id: string, userId: string, connectionId: string, status: ShareStatus = 'live'): ShareInfo {
  return {
    id,
    userId,
    connectionId,
    kind: 'window',
    preset: 'auto',
    audio: true,
    status,
    layers: status === 'starting' ? [] : ['high', 'low'],
    startedAt: '2026-09-30T12:00:00.000Z',
    watchers: [],
  };
}

const me = { userId: 'u_me', connectionId: 'c_this_tab' };

describe('findShareElsewhere (05 §13.1)', () => {
  it('finds a share of the same user from another connection', () => {
    const other = share('s_2', 'u_me', 'c_other_tab');
    expect(findShareElsewhere([share('s_1', 'u_friend', 'c_friend'), other], me)).toBe(other);
  });

  it("ignores this connection's own share and other people's shares", () => {
    expect(findShareElsewhere([], me)).toBeNull();
    expect(
      findShareElsewhere([share('s_1', 'u_me', 'c_this_tab'), share('s_2', 'u_friend', 'c_other_tab')], me),
    ).toBeNull();
  });

  it.each<ShareStatus>(['starting', 'live', 'stalled'])('counts a %s share: it exists', (status) => {
    expect(findShareElsewhere([share('s_1', 'u_me', 'c_phone', status)], me)?.id).toBe('s_1');
  });
});
