// viewerStore (05 §6.1): room.state snapshots become tiles and focus events; per-share state follows the shares.
import { describe, expect, it, vi } from 'vitest';

import { room, SELF, shareInfo, status } from './testing';
import { createViewerStore, type ViewerState } from './viewerStore';

const ids = (s: ViewerState): string[] => s.shares.map((x) => x.id);

describe('viewerStore: defaults', () => {
  it('starts in auto-focus with nothing shown, audio locked, the given volume', () => {
    expect(createViewerStore().getState()).toMatchObject({
      shares: [],
      focusedShareId: null,
      focusMode: 'auto',
      audibleShareId: null,
      audio: 'locked',
      volume: 1,
      fullscreen: false,
      pipShareId: null,
      visible: {},
      pageHiddenSince: null,
      status: {},
      media: 'idle',
    });
    expect(createViewerStore({ volume: 0.4 }).getState().volume).toBe(0.4);
    expect(createViewerStore({ volume: 7 }).getState().volume).toBe(1);
  });
});

describe('viewerStore.syncRoom', () => {
  it('gives live and stalled shares a tile, newest first, and none to shares in `starting`', () => {
    const store = createViewerStore();
    store.getState().syncRoom(
      room(
        shareInfo('s_old', 'u_bea', 1),
        shareInfo('s_stalled', 'u_cy', 2, { status: 'stalled' }),
        shareInfo('s_starting', 'u_cy', 3, { status: 'starting', layers: [] }),
        // A status this build doesn't know reads as live (01 §8.13).
        shareInfo('s_future', 'u_bea', 4, { status: 'paused' as never }),
      ),
      SELF,
    );
    const s = store.getState();
    expect(ids(s)).toEqual(['s_future', 's_stalled', 's_old']);
    expect(s.focusedShareId).toBe('s_future');
    expect(s.shares[1]).toMatchObject({ id: 's_stalled', userId: 'u_cy', ownerName: 'Cy', own: false, local: false });
    expect(s.shares[1]?.info.status).toBe('stalled');
  });

  it('adds the tile when a share goes from starting to live, and focuses it', () => {
    const store = createViewerStore();
    const sync = (...shares: Parameters<typeof room>) => {
      store.getState().syncRoom(room(...shares), SELF);
    };
    sync(shareInfo('s_a', 'u_bea', 1), shareInfo('s_b', 'u_cy', 2, { status: 'starting' }));
    expect(store.getState().focusedShareId).toBe('s_a');
    sync(shareInfo('s_a', 'u_bea', 1), shareInfo('s_b', 'u_cy', 2));
    expect(ids(store.getState())).toEqual(['s_b', 's_a']);
    expect(store.getState()).toMatchObject({ focusedShareId: 's_b', audibleShareId: 's_b', focusMode: 'auto' });
  });

  it('marks the shares of this user and of this page', () => {
    const store = createViewerStore();
    store
      .getState()
      .syncRoom(
        room(
          shareInfo('s_bea', 'u_bea', 1),
          shareInfo('s_mine', 'u_alex', 2, { connectionId: 'c_me' }),
          shareInfo('s_my_phone', 'u_alex', 3, { connectionId: 'c_phone' }),
        ),
        SELF,
      );
    const byId = Object.fromEntries(store.getState().shares.map((s) => [s.id, s]));
    expect(byId['s_mine']).toMatchObject({ own: true, local: true, ownerName: 'Alex' });
    expect(byId['s_my_phone']).toMatchObject({ own: true, local: false });
    expect(byId['s_bea']).toMatchObject({ own: false, local: false });
    // Own shares are never focused automatically, however new.
    expect(store.getState().focusedShareId).toBe('s_bea');
  });

  it('holds a pick against a new share and resumes auto-focus when the picked share ends (owner decision)', () => {
    const store = createViewerStore();
    const a = shareInfo('s_a', 'u_bea', 1);
    const b = shareInfo('s_b', 'u_cy', 2);
    const c = shareInfo('s_c', 'u_cy', 3);
    store.getState().syncRoom(room(a, b), SELF);
    store.getState().focusShare('s_a');
    expect(store.getState()).toMatchObject({ focusedShareId: 's_a', audibleShareId: 's_a', focusMode: 'manual' });

    store.getState().syncRoom(room(a, b, c), SELF);
    expect(store.getState()).toMatchObject({ focusedShareId: 's_a', audibleShareId: 's_a', focusMode: 'manual' });
    expect(ids(store.getState())).toEqual(['s_c', 's_b', 's_a']);

    store.getState().syncRoom(room(b, c), SELF);
    expect(store.getState()).toMatchObject({ focusedShareId: 's_c', audibleShareId: 's_c', focusMode: 'auto' });
  });

  it('hands focus, audio and the tile position to a re-published share of the same user', () => {
    const store = createViewerStore();
    const a = shareInfo('s_a', 'u_bea', 1);
    const b = shareInfo('s_b', 'u_cy', 2);
    store.getState().syncRoom(room(a, b), SELF);
    store.getState().focusShare('s_a');
    // The server restarted: both shares are back under new ids, each naming the one it replaces.
    store
      .getState()
      .syncRoom(
        room(shareInfo('s_b2', 'u_cy', 30, { replaces: 's_b' }), shareInfo('s_a2', 'u_bea', 31, { replaces: 's_a' })),
        SELF,
      );
    const s = store.getState();
    expect(ids(s)).toEqual(['s_b2', 's_a2']);
    expect(s).toMatchObject({ focusedShareId: 's_a2', audibleShareId: 's_a2', focusMode: 'manual' });
  });

  it('carries nothing over when `replaces` names another user’s share', () => {
    const store = createViewerStore();
    store.getState().syncRoom(room(shareInfo('s_a', 'u_bea', 1)), SELF);
    store.getState().focusShare('s_a');
    store.getState().syncRoom(room(shareInfo('s_x', 'u_cy', 30, { replaces: 's_a' })), SELF);
    // s_a ended (the pick with it), and s_x is just the newest share.
    expect(store.getState()).toMatchObject({ focusedShareId: 's_x', focusMode: 'auto' });
    expect(ids(store.getState())).toEqual(['s_x']);
  });

  it('keeps the entry of an unchanged share and replaces the one whose details changed', () => {
    const store = createViewerStore();
    store.getState().syncRoom(room(shareInfo('s_a', 'u_bea', 1), shareInfo('s_b', 'u_cy', 2)), SELF);
    const [b1, a1] = store.getState().shares;
    const listener = vi.fn();
    store.subscribe(listener);

    // The same snapshot again (new objects, same content): no update at all.
    store.getState().syncRoom(room(shareInfo('s_a', 'u_bea', 1), shareInfo('s_b', 'u_cy', 2)), SELF);
    expect(listener).not.toHaveBeenCalled();

    store
      .getState()
      .syncRoom(
        room(
          shareInfo('s_a', 'u_bea', 1, { watchers: [{ userId: 'u_alex', video: 'low', audio: 'off' }] }),
          shareInfo('s_b', 'u_cy', 2),
        ),
        SELF,
      );
    const [b2, a2] = store.getState().shares;
    expect(b2).toBe(b1);
    expect(a2).not.toBe(a1);
    expect(a2?.info.watchers).toHaveLength(1);
    expect(store.getState().focusedShareId).toBe('s_b');
  });

  it('re-reads whose share it is when this page’s identity changes', () => {
    const store = createViewerStore();
    const mine = shareInfo('s_mine', 'u_alex', 1, { connectionId: 'c_me' });
    store.getState().syncRoom(room(mine), { userId: 'u_alex', connectionId: 'c_other_tab' });
    expect(store.getState().shares[0]).toMatchObject({ own: true, local: false });
    store.getState().syncRoom(room(mine), SELF);
    expect(store.getState().shares[0]).toMatchObject({ own: true, local: true });
  });

  it('drops the per-share state of shares that are gone', () => {
    const store = createViewerStore();
    const a = shareInfo('s_a', 'u_bea', 1);
    const b = shareInfo('s_b', 'u_cy', 2);
    store.getState().syncRoom(room(a, b), SELF);
    store.getState().setVisible('s_a', true);
    store.getState().setVisible('s_b', true);
    store.getState().applyStatus([status('s_a', { reason: 'waiting' }), status('s_b')]);
    store.getState().setPip('s_a');

    store.getState().syncRoom(room(b), SELF);
    const s = store.getState();
    expect(s.visible).toEqual({ s_b: true });
    expect(Object.keys(s.status)).toEqual(['s_b']);
    expect(s.pipShareId).toBeNull();
  });

  it('empties the tiles when the page is in no room', () => {
    const store = createViewerStore();
    store.getState().syncRoom(room(shareInfo('s_a', 'u_bea', 1)), SELF);
    store.getState().focusShare('s_a');
    store.getState().syncRoom(null, SELF);
    expect(store.getState()).toMatchObject({ shares: [], focusedShareId: null, audibleShareId: null });
    expect(store.getState().focusMode).toBe('auto');
  });

  it('uses an empty name for a sharer the snapshot does not list', () => {
    const store = createViewerStore();
    store.getState().syncRoom({ shares: [shareInfo('s_a', 'u_ghost', 1)], participants: [] }, SELF);
    expect(store.getState().shares[0]?.ownerName).toBe('');
  });
});

describe('viewerStore: actions', () => {
  it('setAudible moves the sound without the focus, only to a share this page receives', () => {
    const store = createViewerStore();
    store
      .getState()
      .syncRoom(
        room(
          shareInfo('s_a', 'u_bea', 1),
          shareInfo('s_b', 'u_cy', 2),
          shareInfo('s_mine', 'u_alex', 3, { connectionId: 'c_me' }),
        ),
        SELF,
      );
    store.getState().setAudible('s_a');
    expect(store.getState()).toMatchObject({ focusedShareId: 's_b', audibleShareId: 's_a', focusMode: 'auto' });
    store.getState().setAudible('s_mine');
    store.getState().setAudible('s_unknown');
    expect(store.getState().audibleShareId).toBe('s_a');
    store.getState().setAudible(null);
    expect(store.getState().audibleShareId).toBeNull();
  });

  it('applyStatus merges entries by share (only changed subscriptions are sent)', () => {
    const store = createViewerStore();
    store.getState().syncRoom(room(shareInfo('s_a', 'u_bea', 1), shareInfo('s_b', 'u_cy', 2)), SELF);
    store.getState().applyStatus([status('s_a', { video: 'low', reason: 'bandwidth' }), status('s_b')]);
    store.getState().applyStatus([status('s_a')]);
    store.getState().applyStatus([]);
    expect(store.getState().status).toEqual({ s_a: status('s_a'), s_b: status('s_b') });
  });

  it('keeps the simple values', () => {
    const store = createViewerStore();
    const s = store.getState();
    s.setAudio('blocked');
    s.setVolume(-3);
    s.setFullscreen(true);
    s.setPip('s_a');
    s.setPageHidden(1234);
    s.setMedia('unreachable');
    s.setVisible('s_a', true);
    s.setVisible('s_a', true);
    expect(store.getState()).toMatchObject({
      audio: 'blocked',
      volume: 0,
      fullscreen: true,
      pipShareId: 's_a',
      pageHiddenSince: 1234,
      media: 'unreachable',
      visible: { s_a: true },
    });
  });

  it('dispatch runs one focus event; a ?focus= share is picked once it is live', () => {
    const store = createViewerStore();
    store.getState().syncRoom(room(shareInfo('s_a', 'u_bea', 1)), SELF);
    store.getState().dispatch({ type: 'focusParam', shareId: 's_b' });
    expect(store.getState()).toMatchObject({ focusedShareId: 's_a', pendingFocusParam: 's_b' });
    store.getState().syncRoom(room(shareInfo('s_a', 'u_bea', 1), shareInfo('s_c', 'u_cy', 5)), SELF);
    expect(store.getState().focusedShareId).toBe('s_c');
    store
      .getState()
      .syncRoom(room(shareInfo('s_a', 'u_bea', 1), shareInfo('s_b', 'u_bea', 2), shareInfo('s_c', 'u_cy', 5)), SELF);
    expect(store.getState()).toMatchObject({ focusedShareId: 's_b', focusMode: 'manual', pendingFocusParam: null });
  });

  it('reset() forgets the room but keeps what belongs to the page', () => {
    const store = createViewerStore({ volume: 0.5 });
    store.getState().syncRoom(room(shareInfo('s_a', 'u_bea', 1)), SELF);
    store.getState().focusShare('s_a');
    store.getState().applyStatus([status('s_a')]);
    store.getState().setVisible('s_a', true);
    store.getState().setFullscreen(true);
    store.getState().setAudio('playing');
    store.getState().setMedia('connected');
    store.getState().setPageHidden(99);

    store.getState().reset();
    expect(store.getState()).toMatchObject({
      shares: [],
      focusedShareId: null,
      focusMode: 'auto',
      audibleShareId: null,
      pendingFocusParam: null,
      fullscreen: false,
      pipShareId: null,
      visible: {},
      status: {},
      // The page's own:
      volume: 0.5,
      audio: 'playing',
      media: 'connected',
      pageHiddenSince: 99,
    });
  });
});
