// The focus reducer (05 §12.2, §19.1): the newest share is focused automatically; a pick holds until its share
// ends, then auto-focus resumes (owner decision, 05 §24.1); `replaces` carries focus and audio only for the same
// user; ?focus=; a share of this user is never focused by itself.
import { describe, expect, it } from 'vitest';

import {
  autoFocusTarget,
  focusReducer,
  initialFocusState,
  type FocusEvent,
  type FocusShare,
  type FocusState,
} from './autoFocus';

/** A share that started `minute` minutes into the hour. */
function share(id: string, minute: number, userId = `u_${id}`, own = false): FocusShare {
  return { id, userId, startedAt: `2026-10-12T19:${String(minute).padStart(2, '0')}:00.000Z`, own };
}

function run(events: FocusEvent[], from: FocusState = initialFocusState()): FocusState {
  return events.reduce(focusReducer, from);
}

const live = (s: FocusShare): FocusEvent => ({ type: 'shareLive', share: s });
const ended = (shareId: string): FocusEvent => ({ type: 'shareEnded', shareId });
const pick = (shareId: string): FocusEvent => ({ type: 'userFocus', shareId });
const ids = (s: FocusState): string[] => s.shares.map((x) => x.id);

const A = share('a', 1);
const B = share('b', 2);
const C = share('c', 3);
const MINE = share('mine', 4, 'me', true);

describe('auto-focus', () => {
  it('starts with nothing on the stage', () => {
    expect(initialFocusState()).toEqual({
      shares: [],
      focusedShareId: null,
      focusMode: 'auto',
      audibleShareId: null,
      pendingFocusParam: null,
    });
  });

  it('focuses the newest share, with its sound', () => {
    let s = run([live(A)]);
    expect(s).toMatchObject({ focusedShareId: 'a', audibleShareId: 'a', focusMode: 'auto' });
    s = run([live(B)], s);
    expect(s).toMatchObject({ focusedShareId: 'b', audibleShareId: 'b', focusMode: 'auto' });
    expect(ids(s)).toEqual(['b', 'a']);
  });

  it('goes by startedAt, whatever order the shares arrive in', () => {
    const s = run([live(C), live(A), live(B)]);
    expect(s.focusedShareId).toBe('c');
    expect(ids(s)).toEqual(['c', 'b', 'a']);
    // Same start: ordered by id, so every viewer shows the same order.
    const tie = run([live(share('y', 5)), live(share('x', 5))]);
    expect(ids(tie)).toEqual(['x', 'y']);
    expect(tie.focusedShareId).toBe('x');
  });

  it('moves on to the newest share left when the focused one ends', () => {
    const s = run([live(A), live(B), live(C), ended('c')]);
    expect(s).toMatchObject({ focusedShareId: 'b', audibleShareId: 'b', focusMode: 'auto' });
    expect(run([ended('b'), ended('a')], s)).toMatchObject({ focusedShareId: null, audibleShareId: null, shares: [] });
  });

  it('keeps the focus when another share ends', () => {
    const s = run([live(A), live(B), ended('a')]);
    expect(s).toMatchObject({ focusedShareId: 'b', audibleShareId: 'b' });
    expect(ids(s)).toEqual(['b']);
  });

  it('never focuses a share of this user by itself', () => {
    let s = run([live(MINE)]);
    expect(s).toMatchObject({ focusedShareId: null, audibleShareId: null });
    expect(ids(s)).toEqual(['mine']);
    s = run([live(A)], s);
    // The own share is newer, yet the stage shows the other person's.
    expect(s.focusedShareId).toBe('a');
    expect(run([ended('a')], s)).toMatchObject({ focusedShareId: null, audibleShareId: null });
    expect(autoFocusTarget([MINE, A, B])).toBe('b');
    expect(autoFocusTarget([MINE])).toBeNull();
  });

  it('updates a known share in place', () => {
    const s = run([live(A), live(B)]);
    const a2 = { ...A };
    const next = run([live(a2)], s);
    expect(next.shares).toEqual([B, a2]);
    expect(next.shares[1]).toBe(a2);
    expect(next.focusedShareId).toBe('b');
    // The same object again changes nothing at all.
    expect(run([live(a2)], next)).toBe(next);
  });

  it('ignores the end of a share it does not know', () => {
    const s = run([live(A)]);
    expect(run([ended('nope')], s)).toBe(s);
  });
});

describe('a pick (owner decision: a new share only gets a toast)', () => {
  it('focuses the picked tile and holds it: a newer share takes neither the stage nor the sound', () => {
    let s = run([live(A), live(B), pick('a')]);
    expect(s).toMatchObject({ focusedShareId: 'a', audibleShareId: 'a', focusMode: 'manual' });
    s = run([live(C)], s);
    expect(s).toMatchObject({ focusedShareId: 'a', audibleShareId: 'a', focusMode: 'manual' });
    expect(ids(s)).toEqual(['c', 'b', 'a']);
    // Other shares ending change nothing either.
    expect(run([ended('b')], s)).toMatchObject({ focusedShareId: 'a', focusMode: 'manual' });
  });

  it('holds until the picked share ends; then auto-focus resumes with the newest live share', () => {
    let s = run([live(A), live(B), pick('a'), live(C), ended('a')]);
    expect(s).toMatchObject({ focusedShareId: 'c', audibleShareId: 'c', focusMode: 'auto' });
    // Back in auto mode, the next new share takes the stage again.
    const D = share('d', 9);
    s = run([live(D)], s);
    expect(s).toMatchObject({ focusedShareId: 'd', audibleShareId: 'd', focusMode: 'auto' });
  });

  it('resumes with an empty stage when nothing else is live', () => {
    expect(run([live(A), pick('a'), ended('a')])).toMatchObject({
      focusedShareId: null,
      audibleShareId: null,
      focusMode: 'auto',
    });
  });

  it('counts a pick of the already focused tile as a pick', () => {
    const s = run([live(A), pick('a'), live(B)]);
    expect(s).toMatchObject({ focusedShareId: 'a', focusMode: 'manual' });
  });

  it('ignores a pick of a share without a tile, and a repeated pick', () => {
    const s = run([live(A)]);
    expect(run([pick('gone')], s)).toBe(s);
    const picked = run([pick('a')], s);
    expect(run([pick('a')], picked)).toBe(picked);
  });

  it('lets the user enlarge their own preview; the sound stays where it was', () => {
    const s = run([live(A), live(MINE), pick('mine')]);
    expect(s).toMatchObject({ focusedShareId: 'mine', focusMode: 'manual', audibleShareId: 'a' });
    // When the own share ends, the stage goes back to the newest share of someone else.
    expect(run([ended('mine')], s)).toMatchObject({ focusedShareId: 'a', audibleShareId: 'a', focusMode: 'auto' });
  });
});

describe('audio follows focus (05 §12.3)', () => {
  /** The speaker button of another tile: heard without being focused. */
  const listenTo = (s: FocusState, shareId: string): FocusState => ({ ...s, audibleShareId: shareId });

  it('keeps the share the user listens to when the focus moves by itself', () => {
    let s = listenTo(run([live(A), live(B)]), 'a');
    s = run([live(C)], s);
    expect(s).toMatchObject({ focusedShareId: 'c', audibleShareId: 'a' });
    s = run([ended('c')], s);
    expect(s).toMatchObject({ focusedShareId: 'b', audibleShareId: 'a' });
  });

  it('moves the sound with a pick', () => {
    const s = listenTo(run([live(A), live(B)]), 'a');
    expect(run([pick('b')], s)).toMatchObject({ focusedShareId: 'b', audibleShareId: 'b', focusMode: 'manual' });
  });

  it('goes back to the focused share when the one listened to ends', () => {
    const s = listenTo(run([live(A), live(B)]), 'a');
    expect(run([ended('a')], s)).toMatchObject({ focusedShareId: 'b', audibleShareId: 'b' });
  });

  it('goes silent when the one listened to ends and the stage shows the own preview', () => {
    const s = listenTo(run([live(A), live(MINE), pick('mine')]), 'a');
    expect(run([ended('a')], s)).toMatchObject({ focusedShareId: 'mine', audibleShareId: null });
  });
});

describe('shareReplaced (01 §10.6)', () => {
  const replaced = (s: FocusShare, replaces: string): FocusEvent => ({ type: 'shareReplaced', share: s, replaces });

  it('moves the focus, the sound and the tile position to the new share of the same user', () => {
    const before = run([live(A), live(B), live(C), pick('a')]);
    const a2 = share('a2', 30, A.userId);
    const s = run([replaced(a2, 'a')], before);
    expect(s).toMatchObject({ focusedShareId: 'a2', audibleShareId: 'a2', focusMode: 'manual' });
    // a2 is the newest by far, but it sits where a was.
    expect(ids(s)).toEqual(['c', 'b', 'a2']);
  });

  it('carries the sound alone when only the sound was on the replaced share', () => {
    const before = { ...run([live(A), live(B)]), audibleShareId: 'a' };
    const s = run([replaced(share('a2', 30, A.userId), 'a')], before);
    expect(s).toMatchObject({ focusedShareId: 'b', audibleShareId: 'a2' });
  });

  it('keeps the place of a replaced share that was neither focused nor heard', () => {
    const before = run([live(A), live(B), live(C)]);
    const s = run([replaced(share('a2', 30, A.userId), 'a')], before);
    expect(ids(s)).toEqual(['c', 'b', 'a2']);
    expect(s).toMatchObject({ focusedShareId: 'c', audibleShareId: 'c', focusMode: 'auto' });
  });

  it('carries nothing over for another user: it is just a new share', () => {
    const before = run([live(A), live(B), pick('a')]);
    const forged = share('x', 30, 'someone-else');
    const s = run([replaced(forged, 'a')], before);
    expect(s).toMatchObject({ focusedShareId: 'a', audibleShareId: 'a', focusMode: 'manual' });
    expect(ids(s)).toEqual(['x', 'b', 'a']);
  });

  it('treats a replacement of a share it never saw as a new share', () => {
    const s = run([live(A), replaced(share('z2', 30, 'u_z'), 'z')]);
    expect(s).toMatchObject({ focusedShareId: 'z2', audibleShareId: 'z2', focusMode: 'auto' });
    expect(ids(s)).toEqual(['z2', 'a']);
  });
});

describe('?focus=<shareId> (05 §12.2)', () => {
  const param = (shareId: string | null): FocusEvent => ({ type: 'focusParam', shareId });

  it('acts as a pick when the share is live', () => {
    const s = run([live(A), live(B), param('a')]);
    expect(s).toMatchObject({ focusedShareId: 'a', audibleShareId: 'a', focusMode: 'manual', pendingFocusParam: null });
  });

  it('waits for the share to go live, then picks it', () => {
    let s = run([live(A), param('b')]);
    expect(s).toMatchObject({ focusedShareId: 'a', focusMode: 'auto', pendingFocusParam: 'b' });
    s = run([live(C)], s);
    expect(s).toMatchObject({ focusedShareId: 'c', pendingFocusParam: 'b' });
    s = run([live(B)], s);
    expect(s).toMatchObject({ focusedShareId: 'b', audibleShareId: 'b', focusMode: 'manual', pendingFocusParam: null });
  });

  it('is dropped when the wait ends', () => {
    let s = run([live(A), param('b'), param(null)]);
    expect(s.pendingFocusParam).toBeNull();
    s = run([live(B)], s);
    expect(s).toMatchObject({ focusedShareId: 'b', focusMode: 'auto' });
    expect(run([param(null)], s)).toBe(s);
  });

  it('gives way to a pick made while waiting', () => {
    const s = run([live(A), live(C), param('b'), pick('a'), live(B)]);
    expect(s).toMatchObject({ focusedShareId: 'a', focusMode: 'manual', pendingFocusParam: null });
  });

  it('picks the awaited share when it arrives as a replacement', () => {
    const s = run([live(A), param('a2'), { type: 'shareReplaced', share: share('a2', 30, A.userId), replaces: 'a' }]);
    expect(s).toMatchObject({ focusedShareId: 'a2', focusMode: 'manual', pendingFocusParam: null });
  });
});
