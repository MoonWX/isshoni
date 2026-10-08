// What a tile says about its share: names (05 §12.6, §16.6) and states (05 §10.4).
import { describe, expect, it } from 'vitest';

import { i18n } from '../i18n';
import {
  badgeText,
  kindLabel,
  overlayText,
  sharerName,
  shareTitle,
  shareView,
  shareWhat,
  watchingText,
} from './shareView';
import { shareInfo, status } from './testing';
import type { ViewerShare } from './viewerStore';

const t = i18n.t.bind(i18n);

function viewerShare(overrides: Partial<ViewerShare> = {}, info = shareInfo('s_a', 'u_bea', 1)): ViewerShare {
  return {
    id: info.id,
    userId: info.userId,
    startedAt: info.startedAt,
    own: false,
    local: false,
    ownerName: 'Bea',
    watchers: [],
    info,
    ...overrides,
  };
}

describe('names', () => {
  it('labels the kind, reading an unknown kind as a screen (01 §8.13)', () => {
    expect(kindLabel(t, 'screen')).toBe('Screen');
    expect(kindLabel(t, 'window')).toBe('Window');
    expect(kindLabel(t, 'tab')).toBe('Tab');
    expect(kindLabel(t, 'hologram' as never)).toBe('Screen');
  });

  it('names the sharer: the name, "You" for this user, "Someone" without a name', () => {
    expect(sharerName(t, viewerShare())).toBe('Bea');
    expect(sharerName(t, viewerShare({ own: true, ownerName: 'Alex' }))).toBe('You');
    expect(sharerName(t, viewerShare({ ownerName: '' }))).toBe('Someone');
  });

  it('titles a share by kind, or by the label the sharer typed', () => {
    expect(shareTitle(t, viewerShare())).toBe("Bea's window");
    expect(shareTitle(t, viewerShare({}, shareInfo('s', 'u_bea', 1, { kind: 'screen' })))).toBe("Bea's screen");
    expect(shareTitle(t, viewerShare({}, shareInfo('s', 'u_bea', 1, { kind: 'tab' })))).toBe("Bea's tab");
    expect(shareTitle(t, viewerShare({}, shareInfo('s', 'u_bea', 1, { label: ' Movie night ' })))).toBe(
      'Bea: Movie night',
    );
    expect(shareTitle(t, viewerShare({ own: true }))).toBe('Your window');
    expect(shareTitle(t, viewerShare({ own: true }, shareInfo('s', 'u_alex', 1, { kind: 'screen' })))).toBe(
      'Your screen',
    );
    expect(shareTitle(t, viewerShare({ own: true }, shareInfo('s', 'u_alex', 1, { kind: 'tab' })))).toBe('Your tab');
    expect(shareWhat(t, viewerShare())).toBe('Window');
    expect(shareWhat(t, viewerShare({}, shareInfo('s', 'u_bea', 1, { label: 'Movie night' })))).toBe('Movie night');
    expect(shareWhat(t, viewerShare({}, shareInfo('s', 'u_bea', 1, { label: '  ' })))).toBe('Window');
  });

  it('counts the watchers', () => {
    const watcher = (userId: string) => ({ userId, video: 'high' as const, audio: 'on' as const });
    expect(watchingText(t, viewerShare())).toBe('0 watching');
    expect(watchingText(t, viewerShare({}, shareInfo('s', 'u_bea', 1, { watchers: [watcher('a')] })))).toBe(
      '1 watching',
    );
    expect(
      watchingText(t, viewerShare({}, shareInfo('s', 'u_bea', 1, { watchers: [watcher('a'), watcher('b')] }))),
    ).toBe('2 watching');
  });
});

describe('shareView (05 §10.4, §12.6)', () => {
  const live = viewerShare();
  const stalled = viewerShare({}, shareInfo('s_a', 'u_bea', 1, { status: 'stalled' }));

  it('is plain while the video plays and nothing is reported', () => {
    expect(shareView(live, undefined, true)).toEqual({ overlay: null, badge: null });
    expect(shareView(live, status('s_a'), true)).toEqual({ overlay: null, badge: null });
  });

  it('is connecting without a video track, or while the server says `waiting`', () => {
    expect(shareView(live, undefined, false)).toEqual({ overlay: 'connecting', badge: null });
    expect(shareView(live, status('s_a', { video: 'off', reason: 'waiting' }), true)).toEqual({
      overlay: 'connecting',
      badge: null,
    });
  });

  it('shows a badge for `bandwidth` and `unavailable`, and a plain one for a reason it does not know', () => {
    expect(shareView(live, status('s_a', { video: 'low', reason: 'bandwidth' }), true).badge).toBe('bandwidth');
    expect(shareView(live, status('s_a', { video: 'low', reason: 'unavailable' }), true).badge).toBe('unavailable');
    expect(shareView(live, status('s_a', { video: 'low', reason: 'moon-phase' as never }), true)).toEqual({
      overlay: null,
      badge: 'reduced',
    });
  });

  it('shows the decoder wait for `codec`, whatever else is true', () => {
    expect(shareView(live, status('s_a', { video: 'off', reason: 'codec' }), false)).toEqual({
      overlay: 'codec',
      badge: null,
    });
    expect(shareView(stalled, status('s_a', { video: 'off', reason: 'codec' }), true).overlay).toBe('codec');
  });

  it('says "connection unstable" over the last frame of a stalled share', () => {
    expect(shareView(stalled, undefined, true)).toEqual({ overlay: 'stalled', badge: null });
    expect(shareView(stalled, undefined, false).overlay).toBe('stalled');
    expect(shareView(stalled, status('s_a', { video: 'low', reason: 'bandwidth' }), true)).toEqual({
      overlay: 'stalled',
      badge: 'bandwidth',
    });
  });

  it('shows a preview of this page’s share as it is; only `stalled` applies', () => {
    const mine = viewerShare({ own: true, local: true });
    expect(shareView(mine, undefined, false)).toEqual({ overlay: null, badge: null });
    expect(shareView(mine, status('s_a', { reason: 'waiting' }), true)).toEqual({ overlay: null, badge: null });
    const mineStalled = viewerShare({ own: true, local: true }, shareInfo('s_a', 'u_alex', 1, { status: 'stalled' }));
    expect(shareView(mineStalled, undefined, true).overlay).toBe('stalled');
  });

  it('has a text for every state', () => {
    expect(overlayText(t, 'connecting')).toBe('Connecting…');
    expect(overlayText(t, 'stalled')).toBe('Connection unstable');
    expect(overlayText(t, 'codec')).toContain('video decoder');
    expect(badgeText(t, 'bandwidth')).toBe('Lower quality (your connection)');
    expect(badgeText(t, 'unavailable')).toBe("The sharer isn't sending this quality right now");
    expect(badgeText(t, 'reduced')).toBe('Lower quality');
  });
});
