// "bo started sharing [Watch]" (05 §12.2, owner decision §24.1): shown only while the user holds a pick; its action
// picks the new share.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createUiStore, type UiStore } from '../app/uiStore';
import type { RoomEvent } from '../protocol/types.gen';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { room, SELF, shareInfo } from './testing';
import { createWatchToast, WATCH_WAIT_MS } from './watchToast';

const BEA = shareInfo('s_bea', 'u_bea', 1);
const CY = shareInfo('s_cy', 'u_cy', 2);
const NEW = shareInfo('s_new', 'u_cy', 9);

function started(overrides: Partial<RoomEvent> = {}): RoomEvent {
  return {
    kind: 'share.started',
    roomId: 'lounge',
    userId: 'u_cy',
    name: 'Cy',
    shareId: 's_new',
    at: '2026-10-12T19:09:00.000Z',
    ...overrides,
  };
}

let viewer: ViewerServices;
let ui: UiStore;
let tap: (e: RoomEvent) => boolean | undefined;

beforeEach(() => {
  vi.useFakeTimers();
  viewer = createViewer();
  ui = createUiStore();
  tap = createWatchToast({ viewer, ui, selfUserId: () => SELF.userId });
  syncRoom(viewer, room(BEA, CY), SELF);
});

afterEach(() => {
  vi.useRealTimers();
});

describe('createWatchToast', () => {
  it('leaves the announcement to the room while nothing is picked: the new share takes the stage', () => {
    expect(tap(started())).toBeUndefined();
    expect(ui.getState().toasts).toEqual([]);
  });

  it('shows "… started sharing [Watch]" while the user holds a pick, and Watch picks the new share', () => {
    viewer.store.getState().focusShare('s_bea');
    syncRoom(viewer, room(BEA, CY, NEW), SELF);
    expect(viewer.store.getState().focusedShareId).toBe('s_bea');

    expect(tap(started())).toBe(true);
    const [toast] = ui.getState().toasts;
    expect(toast).toMatchObject({ kind: 'info', message: 'Cy started sharing' });
    expect(toast?.action?.label).toBe('Watch');

    toast?.action?.run();
    expect(viewer.store.getState()).toMatchObject({
      focusedShareId: 's_new',
      audibleShareId: 's_new',
      focusMode: 'manual',
      pendingFocusParam: null,
    });
    vi.advanceTimersByTime(WATCH_WAIT_MS);
    expect(viewer.store.getState().focusedShareId).toBe('s_new');
  });

  it('waits for a share that room.state has not listed yet', () => {
    viewer.store.getState().focusShare('s_bea');
    expect(tap(started())).toBe(true); // the room.event came before the room.state
    ui.getState().toasts[0]?.action?.run();
    expect(viewer.store.getState()).toMatchObject({ focusedShareId: 's_bea', pendingFocusParam: 's_new' });

    syncRoom(viewer, room(BEA, CY, NEW), SELF);
    expect(viewer.store.getState()).toMatchObject({ focusedShareId: 's_new', pendingFocusParam: null });
  });

  it('drops the pick when the share never shows up', () => {
    viewer.store.getState().focusShare('s_bea');
    tap(started());
    ui.getState().toasts[0]?.action?.run();
    vi.advanceTimersByTime(WATCH_WAIT_MS - 1);
    expect(viewer.store.getState().pendingFocusParam).toBe('s_new');
    vi.advanceTimersByTime(1);
    expect(viewer.store.getState().pendingFocusParam).toBeNull();
    // Too late now: it is just a new share, and the pick still holds.
    syncRoom(viewer, room(BEA, CY, NEW), SELF);
    expect(viewer.store.getState()).toMatchObject({ focusedShareId: 's_bea', focusMode: 'manual' });
  });

  it('is not its business: other events, a re-publish, the user’s own share, an event without a share', () => {
    viewer.store.getState().focusShare('s_bea');
    expect(tap(started({ kind: 'share.stopped' }))).toBeUndefined();
    expect(tap(started({ kind: 'participant.joined', shareId: undefined }))).toBeUndefined();
    expect(tap(started({ replaces: 's_cy' }))).toBeUndefined();
    expect(tap(started({ userId: SELF.userId, name: 'Alex' }))).toBeUndefined();
    expect(tap(started({ shareId: undefined }))).toBeUndefined();
    expect(tap(started({ kind: 'something.new' as never }))).toBeUndefined();
    expect(ui.getState().toasts).toEqual([]);
  });

  it('uses the translator it is given', () => {
    viewer.store.getState().focusShare('s_bea');
    const t = vi.fn((key: string) => `[${key}]`);
    createWatchToast({ viewer, ui, selfUserId: () => null, t: t as never })(started());
    expect(ui.getState().toasts[0]).toMatchObject({ message: '[viewer.toast.started]' });
    expect(ui.getState().toasts[0]?.action?.label).toBe('[viewer.toast.watch]');
    expect(t).toHaveBeenCalledWith('viewer.toast.started', { name: 'Cy' });
  });
});
