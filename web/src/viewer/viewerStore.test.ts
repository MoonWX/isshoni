// viewerStore (05 §6.1): room.state snapshots become tiles and focus events; per-share state follows the shares.
// The re-publish cases follow the snapshots the protocol produces (01 §10.5, §10.6, §11.6).
import { describe, expect, it, vi } from 'vitest';

import type { ShareInfo } from '../protocol/types.gen';
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
      ended: [],
      inRoom: false,
      audio: 'locked',
      videoBlocked: false,
      volume: 1,
      fullscreen: false,
      pipShareId: null,
      visible: {},
      pageHiddenSince: null,
      status: {},
      frozen: {},
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

  describe('re-published shares (01 §10.6)', () => {
    const a = shareInfo('s_a', 'u_bea', 1);
    const b = shareInfo('s_b', 'u_cy', 2);
    const a2 = (overrides: Partial<ShareInfo> = {}) =>
      shareInfo('s_a2', 'u_bea', 30, { replaces: 's_a', ...overrides });
    const b2 = (overrides: Partial<ShareInfo> = {}) => shareInfo('s_b2', 'u_cy', 31, { replaces: 's_b', ...overrides });

    /** A store that shows a and b, with the older one, a, picked. */
    function picked() {
      const store = createViewerStore();
      const sync = (...shares: ShareInfo[]) => {
        store.getState().syncRoom(room(...shares), SELF);
      };
      sync(a, b);
      store.getState().focusShare('s_a');
      return { store, sync };
    }

    it('keeps a pick and its sound across a server restart (01 §11.6)', () => {
      const { store, sync } = picked();
      // The new server's first room.state after room.join lists no shares.
      sync();
      expect(store.getState()).toMatchObject({ shares: [], focusedShareId: null, focusMode: 'auto' });
      // Both sharers re-publish: a share in `starting` has no tile yet.
      sync(a2({ status: 'starting', layers: [] }), b2({ status: 'starting', layers: [] }));
      expect(store.getState()).toMatchObject({ shares: [], focusedShareId: null, audibleShareId: null });
      sync(a2(), b2());
      const s = store.getState();
      expect(ids(s)).toEqual(['s_b2', 's_a2']);
      expect(s).toMatchObject({ focusedShareId: 's_a2', audibleShareId: 's_a2', focusMode: 'manual', ended: [] });
    });

    it('keeps the pick whichever share is live again first', () => {
      const { store, sync } = picked();
      sync();
      sync(a2({ status: 'starting' }), b2());
      // Until the picked share is back, the stage shows what there is.
      expect(store.getState()).toMatchObject({ focusedShareId: 's_b2', audibleShareId: 's_b2', focusMode: 'auto' });
      sync(a2(), b2());
      expect(store.getState()).toMatchObject({ focusedShareId: 's_a2', audibleShareId: 's_a2', focusMode: 'manual' });

      // And the other way round.
      const other = picked();
      other.sync();
      other.sync(a2(), b2({ status: 'starting' }));
      other.sync(a2(), b2());
      expect(other.store.getState()).toMatchObject({
        focusedShareId: 's_a2',
        audibleShareId: 's_a2',
        focusMode: 'manual',
      });
    });

    it('keeps the pick when the sharer was gone for a while and re-published (01 §10.5)', () => {
      const { store, sync } = picked();
      // The server ended a's share; later its sharer is back.
      sync(b);
      expect(store.getState()).toMatchObject({ focusedShareId: 's_b', audibleShareId: 's_b', focusMode: 'auto' });
      sync(b, a2({ status: 'starting' }));
      sync(b, a2());
      const s = store.getState();
      expect(s).toMatchObject({ focusedShareId: 's_a2', audibleShareId: 's_a2', focusMode: 'manual' });
      expect(ids(s)).toEqual(['s_a2', 's_b']);
    });

    it('hands over, with the tile position, when one snapshot swaps the shares', () => {
      const { store, sync } = picked();
      // Here s_a2 re-published last, so it is the newest: each one still sits where the share it replaces was.
      sync(b2(), a2({ startedAt: '2026-10-12T19:40:00.000Z' }));
      const s = store.getState();
      expect(ids(s)).toEqual(['s_b2', 's_a2']);
      expect(s).toMatchObject({ focusedShareId: 's_a2', audibleShareId: 's_a2', focusMode: 'manual' });

      const one = picked();
      one.sync(b, a2());
      expect(ids(one.store.getState())).toEqual(['s_b', 's_a2']);
      expect(one.store.getState()).toMatchObject({ focusedShareId: 's_a2', focusMode: 'manual' });
    });

    it('hands over at once when the replaced share is still listed, and keeps the pick when that one ends', () => {
      const { store, sync } = picked();
      // The server still holds the sharer's old connection: the old share is stalled next to the new one.
      const stalled = { ...a, status: 'stalled' as const };
      sync(stalled, b, a2());
      expect(ids(store.getState())).toEqual(['s_b', 's_a2', 's_a']);
      expect(store.getState()).toMatchObject({ focusedShareId: 's_a2', audibleShareId: 's_a2', focusMode: 'manual' });
      sync(b, a2());
      expect(ids(store.getState())).toEqual(['s_b', 's_a2']);
      expect(store.getState()).toMatchObject({ focusedShareId: 's_a2', audibleShareId: 's_a2', focusMode: 'manual' });
    });

    it('lets a pick made between the end and the re-publish win', () => {
      const { store, sync } = picked();
      sync(b);
      store.getState().focusShare('s_b');
      sync(b, a2());
      const s = store.getState();
      expect(s).toMatchObject({ focusedShareId: 's_b', audibleShareId: 's_b', focusMode: 'manual' });
      expect(ids(s)).toEqual(['s_a2', 's_b']);
    });

    it('carries nothing over when `replaces` names another user’s share', () => {
      const { store, sync } = picked();
      sync(b, shareInfo('s_x', 'u_cy', 30, { replaces: 's_a' }));
      // s_a ended (the pick with it), and s_x is just the newest share.
      expect(store.getState()).toMatchObject({ focusedShareId: 's_x', audibleShareId: 's_x', focusMode: 'auto' });
      expect(ids(store.getState())).toEqual(['s_x', 's_b']);

      // The same when s_a ended in an earlier snapshot.
      const later = picked();
      later.sync(b);
      later.sync(b, shareInfo('s_x', 'u_cy', 30, { replaces: 's_a' }));
      expect(later.store.getState()).toMatchObject({ focusedShareId: 's_x', audibleShareId: 's_x', focusMode: 'auto' });
    });

    it('gives the sound back to a share heard through its speaker button, unless the user chose another since', () => {
      const c = shareInfo('s_c', 'u_cy', 3);
      const listening = () => {
        const store = createViewerStore();
        const sync = (...shares: ShareInfo[]) => {
          store.getState().syncRoom(room(...shares), SELF);
        };
        sync(a, b, c);
        store.getState().setAudible('s_a');
        sync(b, c);
        expect(store.getState()).toMatchObject({ focusedShareId: 's_c', audibleShareId: 's_c' });
        return { store, sync };
      };

      const back = listening();
      back.sync(b, c, a2());
      expect(back.store.getState()).toMatchObject({ focusedShareId: 's_c', audibleShareId: 's_a2', focusMode: 'auto' });

      const chosen = listening();
      chosen.store.getState().setAudible('s_b');
      chosen.sync(b, c, a2());
      expect(chosen.store.getState()).toMatchObject({ focusedShareId: 's_c', audibleShareId: 's_b' });

      // Pressing the speaker button of the share that plays already is a choice too.
      const same = listening();
      same.store.getState().setAudible('s_c');
      same.sync(b, c, a2());
      expect(same.store.getState()).toMatchObject({ focusedShareId: 's_c', audibleShareId: 's_c' });
    });

    it('keeps the pick when the page was in no room in between: syncRoom(null) is not reset()', () => {
      const { store, sync } = picked();
      store.getState().syncRoom(null, SELF);
      expect(store.getState()).toMatchObject({ shares: [], focusedShareId: null, focusMode: 'auto' });
      sync(a2(), b2());
      expect(store.getState()).toMatchObject({ focusedShareId: 's_a2', audibleShareId: 's_a2', focusMode: 'manual' });
    });

    it('forgets the ended shares on reset()', () => {
      const { store, sync } = picked();
      sync();
      expect(store.getState().ended).toHaveLength(2);
      store.getState().reset();
      expect(store.getState().ended).toEqual([]);
      sync(a2());
      expect(store.getState()).toMatchObject({ focusedShareId: 's_a2', focusMode: 'auto' });
    });
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

  it('names who watches each share, for the watchers popover (05 §12.6)', () => {
    const store = createViewerStore();
    const watchedBy = (...userIds: string[]) =>
      room(
        shareInfo('s_a', 'u_bea', 1, {
          watchers: userIds.map((userId) => ({ userId, video: 'low' as const, audio: 'off' as const })),
        }),
      );
    store.getState().syncRoom(watchedBy('u_cy', 'u_alex', 'u_gone'), SELF);
    const first = store.getState().shares[0];
    expect(first?.watchers).toEqual([
      { userId: 'u_cy', name: 'Cy', self: false },
      { userId: 'u_alex', name: 'Alex', self: true },
      { userId: 'u_gone', name: '', self: false }, // someone the snapshot doesn't list
    ]);

    // The same watchers again: the entry is kept, so the tile doesn't render again.
    store.getState().syncRoom(watchedBy('u_cy', 'u_alex', 'u_gone'), SELF);
    expect(store.getState().shares[0]).toBe(first);

    // A watcher's name changed (or arrived with a later snapshot): a new entry.
    const renamed = watchedBy('u_cy', 'u_alex', 'u_gone');
    store.getState().syncRoom(
      {
        ...renamed,
        participants: [...renamed.participants.filter((p) => p.userId !== 'u_cy'), { userId: 'u_cy', name: 'Cyrus' }],
      },
      SELF,
    );
    expect(store.getState().shares[0]).not.toBe(first);
    expect(store.getState().shares[0]?.watchers.map((w) => w.name)).toEqual(['Cyrus', 'Alex', '']);
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
    store.getState().setFrozen(['s_a', 's_b']);
    store.getState().setPip('s_a');

    store.getState().syncRoom(room(b), SELF);
    const s = store.getState();
    expect(s.visible).toEqual({ s_b: true });
    expect(Object.keys(s.status)).toEqual(['s_b']);
    expect(s.frozen).toEqual({ s_b: true });
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

  it('knows whether a room.state stands behind the shares (inRoom), and says it with them in one update', () => {
    const store = createViewerStore();
    const seen: [boolean, number][] = [];
    store.subscribe((s) => seen.push([s.inRoom, s.shares.length]));

    // A room without a share is a room: its first snapshot is what a `?focus=` link waits for.
    store.getState().syncRoom(room(), SELF);
    expect(store.getState().inRoom).toBe(true);
    store.getState().syncRoom(room(), SELF); // nothing new: nobody is told
    store.getState().syncRoom(room(shareInfo('s_a', 'u_bea', 1)), SELF);
    store.getState().dispatch({ type: 'userFocus', shareId: 's_a' }); // an event without a snapshot leaves it
    expect(store.getState().inRoom).toBe(true);
    store.getState().syncRoom(null, SELF);
    expect(store.getState().inRoom).toBe(false);
    store.getState().syncRoom(null, SELF);
    store.getState().syncRoom(room(shareInfo('s_b', 'u_cy', 2)), SELF);
    expect(seen).toEqual([
      [true, 0],
      [true, 1],
      [true, 1], // the pick
      [false, 0],
      [true, 1],
    ]);
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

  it('toggleAudible is the speaker button: hear that share; pressed again, the sound goes back to the stage', () => {
    const store = createViewerStore();
    const laptop = shareInfo('s_laptop', 'u_alex', 3, { connectionId: 'c_laptop' });
    store.getState().syncRoom(room(shareInfo('s_a', 'u_bea', 1), shareInfo('s_b', 'u_cy', 2), laptop), SELF);
    expect(store.getState()).toMatchObject({ focusedShareId: 's_b', audibleShareId: 's_b' });

    store.getState().toggleAudible('s_a');
    expect(store.getState()).toMatchObject({ focusedShareId: 's_b', audibleShareId: 's_a', focusMode: 'auto' });
    // Another tile's button: exactly one share is heard.
    store.getState().toggleAudible('s_laptop');
    expect(store.getState().audibleShareId).toBe('s_laptop');
    store.getState().toggleAudible('s_laptop');
    expect(store.getState()).toMatchObject({ focusedShareId: 's_b', audibleShareId: 's_b', focusMode: 'auto' });

    // On the stage's own share it turns the sound off and on.
    store.getState().toggleAudible('s_b');
    expect(store.getState().audibleShareId).toBeNull();
    store.getState().toggleAudible('s_b');
    expect(store.getState().audibleShareId).toBe('s_b');

    // With a share of this user on the stage there is nothing to go back to: focus never makes it audible.
    store.getState().focusShare('s_laptop');
    expect(store.getState()).toMatchObject({ focusedShareId: 's_laptop', audibleShareId: 's_b' });
    store.getState().toggleAudible('s_b');
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

  it('setFrozen takes the complete set of frozen tiles, and only shares that have one', () => {
    const store = createViewerStore();
    store.getState().syncRoom(room(shareInfo('s_a', 'u_bea', 1), shareInfo('s_b', 'u_cy', 2)), SELF);
    store.getState().setFrozen(['s_a', 's_gone']);
    expect(store.getState().frozen).toEqual({ s_a: true });
    store.getState().setFrozen(['s_b']);
    expect(store.getState().frozen).toEqual({ s_b: true });

    // The same set again is no change: nothing that reads the store runs.
    const changes = vi.fn();
    const off = store.subscribe(changes);
    store.getState().setFrozen(['s_b']);
    store.getState().setFrozen(['s_b', 's_gone']);
    expect(changes).not.toHaveBeenCalled();
    store.getState().setFrozen([]);
    expect(changes).toHaveBeenCalledTimes(1);
    expect(store.getState().frozen).toEqual({});
    off();
  });

  it('says nothing when a value is set to what it is: fullscreen, PiP, the hidden page', () => {
    const store = createViewerStore();
    const changes = vi.fn();
    const off = store.subscribe(changes);
    store.getState().setFullscreen(false);
    store.getState().setPip(null);
    store.getState().setPageHidden(null);
    expect(changes).not.toHaveBeenCalled();
    store.getState().setFullscreen(true);
    store.getState().setFullscreen(true);
    store.getState().setPip('s_a');
    store.getState().setPip('s_a');
    store.getState().setPageHidden(5);
    store.getState().setPageHidden(5);
    expect(changes).toHaveBeenCalledTimes(3);
    off();
  });

  it('keeps the simple values', () => {
    const store = createViewerStore();
    const s = store.getState();
    s.setAudio('blocked');
    s.setVideoBlocked(true);
    s.setVolume(-3);
    s.setFullscreen(true);
    s.setPip('s_a');
    s.setPageHidden(1234);
    s.setMedia('unreachable');
    s.setVisible('s_a', true);
    s.setVisible('s_a', true);
    expect(store.getState()).toMatchObject({
      audio: 'blocked',
      videoBlocked: true,
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
    store.getState().setFrozen(['s_a']);
    store.getState().setFullscreen(true);
    store.getState().setPip('s_a');
    store.getState().setAudio('playing');
    store.getState().setVideoBlocked(true);
    store.getState().setMedia('connected');
    store.getState().setPageHidden(99);

    expect(store.getState().inRoom).toBe(true);
    store.getState().reset();
    expect(store.getState()).toMatchObject({
      shares: [],
      focusedShareId: null,
      focusMode: 'auto',
      audibleShareId: null,
      pendingFocusParam: null,
      ended: [],
      inRoom: false,
      fullscreen: false,
      pipShareId: null,
      visible: {},
      status: {},
      frozen: {},
      // The page's own:
      volume: 0.5,
      audio: 'playing',
      videoBlocked: true,
      media: 'connected',
      pageHiddenSince: 99,
    });
  });
});
