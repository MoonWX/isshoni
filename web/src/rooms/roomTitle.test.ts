// The room page's document title (05 §11.2): "● 2 live · Lounge · isshoni", and "● Sharing · …" while you share.
import { describe, expect, it } from 'vitest';

import { i18n } from '../i18n';
import { liveShareCount, roomTitle } from './roomTitle';
import { roomState, shareInfo } from './testing/harness';

const t = i18n.t.bind(i18n);

describe('roomTitle', () => {
  it('is the room and the app while nobody shares', () => {
    expect(roomTitle(t, { room: 'Lounge', live: 0, sharing: false })).toBe('Lounge · isshoni');
  });

  it('counts the live shares', () => {
    expect(roomTitle(t, { room: 'Lounge', live: 1, sharing: false })).toBe('● 1 live · Lounge · isshoni');
    expect(roomTitle(t, { room: 'Lounge', live: 2, sharing: false })).toBe('● 2 live · Lounge · isshoni');
  });

  it('says "Sharing" while this page shares, whoever else does', () => {
    expect(roomTitle(t, { room: 'Lounge', live: 0, sharing: true })).toBe('● Sharing · Lounge · isshoni');
    expect(roomTitle(t, { room: 'Lounge', live: 3, sharing: true })).toBe('● Sharing · Lounge · isshoni');
  });

  it('takes the room’s name as it is', () => {
    expect(roomTitle(t, { room: '🎬 Movie night', live: 0, sharing: false })).toBe('🎬 Movie night · isshoni');
    expect(roomTitle(t, { room: 'A & B <1>', live: 0, sharing: false })).toBe('A & B <1> · isshoni');
  });
});

describe('liveShareCount', () => {
  it('counts the shares that have a tile: live and stalled, not starting (01 §4.4)', () => {
    const state = roomState('lounge', 1, {
      shares: [
        shareInfo('s1', 'u1', 'c1'),
        shareInfo('s2', 'u2', 'c2', { status: 'stalled' }),
        shareInfo('s3', 'u3', 'c3', { status: 'starting' }),
      ],
    });
    expect(liveShareCount(state)).toBe(2);
  });

  it('is 0 without a snapshot', () => {
    expect(liveShareCount(null)).toBe(0);
    expect(liveShareCount(roomState('lounge', 1))).toBe(0);
  });
});
