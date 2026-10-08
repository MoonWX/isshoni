// `?focus=<shareId>` (05 §12.2): a manual focus once the share is live, waited for up to 5 s (05 §18).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { FOCUS_PARAM, FOCUS_PARAM_WAIT_MS, followFocusParam } from './focusParam';
import { createViewer, syncRoom, type ViewerServices } from './services';
import { room, SELF, shareInfo } from './testing';

const BEA = shareInfo('s_bea', 'u_bea', 1);
const CY = shareInfo('s_cy', 'u_cy', 2);
const DEE = shareInfo('s_dee', 'u_dee', 3);

let viewer: ViewerServices;
let done: ReturnType<typeof vi.fn<() => void>>;
const state = () => viewer.store.getState();

beforeEach(() => {
  vi.useFakeTimers();
  viewer = createViewer();
  done = vi.fn();
});

afterEach(() => {
  viewer.dispose();
  vi.useRealTimers();
});

describe('followFocusParam', () => {
  it('is the `focus` parameter, waited for 5 s', () => {
    expect(FOCUS_PARAM).toBe('focus');
    expect(FOCUS_PARAM_WAIT_MS).toBe(5_000);
  });

  it('picks a share that is live already, as a manual focus, and is done at once', () => {
    syncRoom(viewer, room(BEA, CY), SELF);
    expect(state().focusedShareId).toBe('s_cy');
    followFocusParam(viewer.store, 's_bea', done);
    expect(state()).toMatchObject({ focusedShareId: 's_bea', focusMode: 'manual', audibleShareId: 's_bea' });
    expect(done).toHaveBeenCalledOnce();
    // Nothing is left running.
    vi.advanceTimersByTime(FOCUS_PARAM_WAIT_MS);
    expect(done).toHaveBeenCalledOnce();
    expect(state().pendingFocusParam).toBeNull();
  });

  it('waits for a share that isn’t live yet, and picks it when it is', () => {
    syncRoom(viewer, room(CY), SELF);
    followFocusParam(viewer.store, 's_bea', done);
    expect(state()).toMatchObject({ focusedShareId: 's_cy', focusMode: 'auto', pendingFocusParam: 's_bea' });
    expect(done).not.toHaveBeenCalled();

    // room.state lists it as starting first: no tile, still waiting.
    syncRoom(viewer, room({ ...BEA, status: 'starting' }, CY), SELF);
    expect(done).not.toHaveBeenCalled();
    vi.advanceTimersByTime(FOCUS_PARAM_WAIT_MS - 1);
    syncRoom(viewer, room(BEA, CY), SELF);
    expect(state()).toMatchObject({ focusedShareId: 's_bea', focusMode: 'manual', pendingFocusParam: null });
    expect(done).toHaveBeenCalledOnce();
  });

  it('gives up after 5 s: the stage stays as it was, and a share that shows up later is not picked', () => {
    syncRoom(viewer, room(CY), SELF);
    followFocusParam(viewer.store, 's_bea', done);
    vi.advanceTimersByTime(FOCUS_PARAM_WAIT_MS - 1);
    expect(done).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(done).toHaveBeenCalledOnce();
    expect(state()).toMatchObject({ focusedShareId: 's_cy', focusMode: 'auto', pendingFocusParam: null });

    syncRoom(viewer, room(shareInfo('s_bea', 'u_bea', 0), CY), SELF);
    expect(state().focusedShareId).toBe('s_cy');
  });

  it('asks again when the store was emptied meanwhile: the link named another room', () => {
    syncRoom(viewer, room(CY), SELF);
    followFocusParam(viewer.store, 's_bea', done);
    // The session switches rooms: the old room's snapshot goes, the store is reset, the new room's arrives.
    syncRoom(viewer, null, SELF);
    state().reset();
    expect(state().pendingFocusParam).toBe('s_bea');
    syncRoom(viewer, room(BEA, DEE), SELF);
    expect(state()).toMatchObject({ focusedShareId: 's_bea', focusMode: 'manual' });
    expect(done).toHaveBeenCalledOnce();
  });

  it('ends when the user picks something else: their choice wins', () => {
    syncRoom(viewer, room(CY, DEE), SELF);
    followFocusParam(viewer.store, 's_bea', done);
    state().focusShare('s_cy');
    expect(done).toHaveBeenCalledOnce();
    syncRoom(viewer, room(BEA, CY, DEE), SELF);
    expect(state()).toMatchObject({ focusedShareId: 's_cy', focusMode: 'manual' });
  });

  it('ends when a newer request for another share takes its place (the toast’s "Watch")', () => {
    syncRoom(viewer, room(CY), SELF);
    followFocusParam(viewer.store, 's_bea', done);
    state().dispatch({ type: 'focusParam', shareId: 's_dee' });
    expect(done).toHaveBeenCalledOnce();
    expect(state().pendingFocusParam).toBe('s_dee');
    // Its own timer no longer touches the store.
    vi.advanceTimersByTime(FOCUS_PARAM_WAIT_MS);
    expect(state().pendingFocusParam).toBe('s_dee');
    expect(done).toHaveBeenCalledOnce();
  });

  it('can be given up without `done`: the pending id goes, and nothing runs afterwards', () => {
    syncRoom(viewer, room(CY), SELF);
    const stop = followFocusParam(viewer.store, 's_bea', done);
    stop();
    expect(state().pendingFocusParam).toBeNull();
    syncRoom(viewer, room(BEA, CY), SELF);
    vi.advanceTimersByTime(FOCUS_PARAM_WAIT_MS);
    expect(state().focusedShareId).toBe('s_cy');
    expect(done).not.toHaveBeenCalled();
    stop(); // twice is once
  });

  it('enlarges the own preview when the link names this page’s share, without moving the sound', () => {
    syncRoom(viewer, room(CY, shareInfo('s_mine', 'u_alex', 3, { connectionId: 'c_me' })), SELF);
    followFocusParam(viewer.store, 's_mine', done);
    expect(state()).toMatchObject({ focusedShareId: 's_mine', focusMode: 'manual', audibleShareId: 's_cy' });
    expect(done).toHaveBeenCalledOnce();
  });
});
