// RoomSession (05 §11.1, §19.1) on the real SignalClient against the fake signaling server: join, leave and the
// resync of 01 §10.5 (resumed and not), room.state and its rev rule, room.event announcements, the local share's
// server-ended transitions (05 §13.1), room-scope errors, and the routing of the sub PC's messages.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { LocalError } from '../lib/errors';
import { ProtocolError } from '../protocol/errors';
import { BackoffMaxMs } from '../protocol/signal-client';
import { makeError } from '../protocol/testing';
import type { EndReason, PCOffer, RoomEvent, SubscriptionWant } from '../protocol/types.gen';
import type { RoomStoreState } from './roomStore';
import { participantOf, sharesOfConnection } from './roomStore';
import { SUBSCRIBE_DEBOUNCE_MS } from './subscriptionSync';
import {
  bareShare,
  createHarness,
  FakeShare,
  participant,
  pickedSource,
  roomState,
  shareInfo,
  tick,
  track,
  type Harness,
} from './testing/harness';

let h: Harness;

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(Math, 'random').mockReturnValue(0.5); // no backoff jitter
  h = createHarness();
});

afterEach(() => {
  h.close();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

const session = () => h.runtime.session;
const room = (): RoomStoreState => h.runtime.stores.room.getState();
const ownConnection = (): string => h.server.welcomes.at(-1)?.connectionId ?? '';
const ownUser = 'k3m9p2qxw7ht'; // the fixtures' welcome.user.id

/** Joins a room like the room page does: the desired room first, then the connection. */
async function inRoom(roomId = 'lounge'): Promise<void> {
  const joined = session().join(roomId);
  await h.connect();
  await joined;
  expect(room().joinState).toBe('joined');
  expect(h.hub.joins).toEqual([roomId]);
}

/**
 * Drops the socket and waits for the next welcome. resumed false: the server forgot the connection (a restart).
 * roomId: the welcome says the connection is still in that room, and its room.state follows like the hub's.
 */
async function reconnect(opts: { resumed?: boolean; roomId?: string } = {}): Promise<void> {
  if (opts.resumed === false) h.server.restart();
  h.server.welcomeDefaults = opts.roomId !== undefined ? { roomId: opts.roomId } : {};
  h.server.drop();
  await tick(BackoffMaxMs);
  expect(h.runtime.signal.state).toBe('ready');
  expect(h.server.welcomes.at(-1)?.resumed).toBe(opts.resumed !== false);
}

/**
 * Drops the socket and reconnects up to the hello. The test then sends the welcome itself (h.server.welcome()), and
 * what the server sends right behind it, before the client's answers to that welcome are read.
 */
async function reconnectToHello(): Promise<void> {
  h.server.autoWelcome = false;
  h.server.drop();
  await tick(1_000);
  expect(h.runtime.signal.state).toBe('handshaking');
  h.server.autoWelcome = true;
}

/** The session's own messages on the newest socket, in order (no hello, no pings). */
const sentOnSocket = (): string[] =>
  h.server
    .messages(undefined, h.server.socket)
    .map((m) => m.type)
    .filter((type) => type !== 'hello' && type !== 'ping');

async function startShare(share?: FakeShare): Promise<FakeShare> {
  const s = share ?? new FakeShare('s_local');
  h.sharing.next.push(s);
  await session().startShare(pickedSource, { preset: 'auto', withAudio: true });
  expect(session().share).toBe(s);
  return s;
}

const event = (kind: string, overrides: Partial<RoomEvent> = {}): RoomEvent =>
  ({
    kind,
    roomId: 'lounge',
    userId: 'b8f2n4r6t0vz',
    name: 'Bea',
    at: '2026-10-12T19:41:07.500Z',
    ...overrides,
  }) as RoomEvent;

const subOffer = (gen: number, neg = 1): PCOffer => ({ pc: 'sub', gen, neg, sdp: 'v=0', tracks: [] });
const wants = (n: number): SubscriptionWant[] =>
  Array.from({ length: n }, (_, i) => ({ shareId: `s${String(i)}`, video: 'low', audio: 'off' }));

describe('join', () => {
  it('stores the desired room first; before ready, the first welcome’s resync joins it', async () => {
    const joined = track(session().join('lounge'));
    expect(session().roomId).toBe('lounge');
    expect(room()).toMatchObject({ roomId: 'lounge', joinState: 'joining', state: null });
    expect(h.hub.joins).toEqual([]);

    await h.connect();
    expect(h.hub.joins).toEqual(['lounge']);
    expect(joined).toEqual({ done: true, value: undefined });
    expect(room()).toMatchObject({
      roomId: 'lounge',
      joinState: 'joined',
      joinError: null,
      room: { id: 'lounge', name: 'Lounge' },
      connectionId: ownConnection(),
      userId: ownUser,
    });
    // ok is followed by room.state (01 §7).
    expect(room().state).toMatchObject({ roomId: 'lounge', rev: 1 });
    // 01 §10.5 reads this key for rejoining.
    expect(h.platform.storage.local.get('isshoni.lastRoomId')).toBe('lounge');
  });

  it('sends room.join at once while ready, and nothing for the room it is already in', async () => {
    await inRoom('lounge');
    const joined = track(session().join('games'));
    expect(h.hub.joins).toEqual(['lounge', 'games']);
    await tick();
    expect(joined).toEqual({ done: true, value: undefined });
    expect(room()).toMatchObject({ roomId: 'games', joinState: 'joined', room: { id: 'games', name: 'Games' } });

    const again = track(session().join('games'));
    await tick();
    expect(again).toEqual({ done: true, value: undefined });
    expect(h.hub.joins).toEqual(['lounge', 'games']);
  });

  it('shares one room.join between callers that ask for the same room', async () => {
    await inRoom('lounge');
    const a = track(session().join('games'));
    const b = track(session().join('games'));
    expect([a.done, b.done]).toEqual([false, false]);
    await tick();
    expect(h.hub.joins).toEqual(['lounge', 'games']);
    expect([a.done, b.done]).toEqual([true, true]);
  });

  it('room_full: rejects, and the store says why (the page shows it)', async () => {
    await inRoom('lounge');
    const full = makeError('room_full', 'request', { params: { limit: 10 } });
    h.server.handle('room.join', () => ({ error: full }));
    const joined = track(session().join('games'));
    await tick();
    expect(joined.error).toBeInstanceOf(ProtocolError);
    expect(room()).toMatchObject({ roomId: 'games', joinState: 'failed', redirect: null });
    expect(room().joinError?.code).toBe('room_full');
    expect(h.toasts()).toEqual([]);
    // The refused join left the connection in lounge on the server, whose UI and media the session already left:
    // it leaves it there too.
    expect(h.hub.sent('room.leave')).toHaveLength(1);
    // The room stays wanted: asking again tries again.
    h.server.handle('room.join', () => ({ ok: { room: { id: 'games', name: 'Games' } } }));
    await session().join('games');
    expect(room()).toMatchObject({ joinState: 'joined', joinError: null });
  });

  it('room_not_found: a toast, and the UI is sent to the default room', async () => {
    await inRoom('lounge');
    h.server.handle('room.join', () => ({ error: makeError('room_not_found', 'request') }));
    const joined = track(session().join('gone'));
    await tick();
    expect((joined.error as ProtocolError).code).toBe('room_not_found');
    expect(h.toasts()).toEqual(["That room doesn't exist anymore."]);
    expect(room()).toMatchObject({
      roomId: null,
      joinState: 'idle',
      state: null,
      redirect: { roomId: 'lounge', code: 'room_not_found' },
    });
    expect(session().roomId).toBeNull();
    // Out of the old room on the server too, so the join that follows the redirect starts clean.
    expect(h.hub.sent('room.leave')).toHaveLength(1);
    room().clearRedirect();
    expect(room().redirect).toBeNull();
    h.server.handle('room.join', (data) => ({ ok: { room: { id: data.roomId, name: 'Lounge' } } }));
    await session().join('lounge');
    const sent = h.server.messages().map((m) => m.type);
    expect(sent.lastIndexOf('room.leave')).toBeLessThan(sent.lastIndexOf('room.join'));
  });

  it('room_not_found for the default room itself: a failed join, no redirect to itself', async () => {
    h.server.handle('room.join', () => ({ error: makeError('room_not_found', 'request') }));
    const joined = track(session().join('lounge'));
    await h.connect();
    expect(joined.error).toBeInstanceOf(ProtocolError);
    expect(room()).toMatchObject({ roomId: 'lounge', joinState: 'failed', redirect: null });
  });

  it('rate_limited: one retry after retryAfterMs', async () => {
    await inRoom('lounge');
    let calls = 0;
    h.server.handle('room.join', (data) =>
      calls++ === 0
        ? { error: makeError('rate_limited', 'request', { retryAfterMs: 3_000 }) }
        : { ok: { room: { id: data.roomId, name: 'Games' } } },
    );
    const joined = track(session().join('games'));
    await tick(2_999);
    expect(h.hub.joins).toEqual(['lounge', 'games']);
    expect(joined.done).toBe(false);
    await tick(1);
    expect(h.hub.joins).toEqual(['lounge', 'games', 'games']);
    expect(joined).toEqual({ done: true, value: undefined });
  });

  it('a retry that is no longer wanted is not sent', async () => {
    await inRoom('lounge');
    h.server.handle('room.join', (data) =>
      data.roomId === 'games'
        ? { error: makeError('rate_limited', 'request', { retryAfterMs: 3_000 }) }
        : { ok: { room: { id: data.roomId, name: 'Lounge' } } },
    );
    void session().join('games');
    await tick(1_000);
    await session().join('lounge');
    await tick(5_000);
    // A late room.join for games would have moved the connection out of lounge.
    expect(h.hub.joins).toEqual(['lounge', 'games', 'lounge']);
    expect(room()).toMatchObject({ roomId: 'lounge', joinState: 'joined' });
  });

  it('internal: one retry at once, then a toast with the reference', async () => {
    await inRoom('lounge');
    h.server.handle('room.join', () => ({ error: makeError('internal', 'request', { params: { ref: 'a1b2c3d4' } }) }));
    const joined = track(session().join('games'));
    await tick();
    expect(h.hub.joins).toEqual(['lounge', 'games', 'games']);
    expect(joined.error).toBeInstanceOf(ProtocolError);
    expect(room().joinState).toBe('failed');
    expect(h.toasts()).toEqual(['Something went wrong on the server. Reference: a1b2c3d4']);
  });

  it('never surfaces connection_lost: the join happens after the next welcome', async () => {
    await inRoom('lounge');
    const joined = track(session().join('games'));
    h.server.drop(); // before the ok
    await tick();
    expect(joined.done).toBe(false);
    expect(room()).toMatchObject({ roomId: 'games', joinState: 'joining', joinError: null });
    await tick(BackoffMaxMs);
    expect(h.hub.joins).toEqual(['lounge', 'games', 'games']);
    expect(joined).toEqual({ done: true, value: undefined });
    expect(room().joinState).toBe('joined');
  });

  it('a later join() replaces an earlier one: its reply is ignored and its promise resolves', async () => {
    await inRoom('lounge');
    h.hub.names['movies'] = 'Movies';
    const second = track(session().join('movies'));
    const third = track(session().join('games'));
    await tick();
    expect(h.hub.joins).toEqual(['lounge', 'movies', 'games']);
    expect([second, third]).toEqual([
      { done: true, value: undefined },
      { done: true, value: undefined },
    ]);
    expect(room()).toMatchObject({ roomId: 'games', joinState: 'joined', room: { id: 'games', name: 'Games' } });
    // The room.state of movies came too, and was not this room's.
    expect(room().state?.roomId).toBe('games');
    expect(h.platform.storage.local.get('isshoni.lastRoomId')).toBe('games');
  });

  it('back to the room before the switch away got through: room.leave first, so the join makes a new MediaPeer', async () => {
    await inRoom('lounge');
    h.server.send('pc.offer', subOffer(3));
    h.server.handle('room.join', (data) =>
      data.roomId === 'games'
        ? { error: makeError('rate_limited', 'request', { retryAfterMs: 3_000 }) }
        : { ok: { room: { id: data.roomId, name: 'Lounge' } } },
    );
    // The switch ended lounge's media on the page; on the server the connection is still in lounge.
    void session().join('games');
    await tick(1_000);
    expect(h.subscribers[0]?.closed).toBe(true);

    const back = session().join('lounge');
    // What lounge's old MediaPeer still sends is not for the next sub PC.
    h.server.send('pc.offer', subOffer(3, 2));
    await back;
    // A room.join alone would have been answered ok with the old MediaPeer kept (01 §8.4).
    expect(sentOnSocket().slice(-2)).toEqual(['room.leave', 'room.join']);
    expect(room()).toMatchObject({ roomId: 'lounge', joinState: 'joined' });
    expect(h.subscribers).toHaveLength(1);
    h.server.send('pc.offer', subOffer(1));
    expect(h.subscribers[1]?.offers).toEqual([subOffer(1)]);
  });

  it('a room.leave that fails before such a join fails the join, and the next try leaves first again', async () => {
    await inRoom('lounge');
    h.server.handle('room.join', (data) =>
      data.roomId === 'games'
        ? { error: makeError('rate_limited', 'request', { retryAfterMs: 3_000 }) }
        : { ok: { room: { id: data.roomId, name: 'Lounge' } } },
    );
    void session().join('games');
    await tick(1_000);
    h.server.handle('room.leave', () => ({ error: makeError('internal', 'request', { params: { ref: 'a1b2c3d4' } }) }));
    const back = track(session().join('lounge'));
    await tick();
    expect((back.error as ProtocolError).code).toBe('internal');
    expect(room()).toMatchObject({ roomId: 'lounge', joinState: 'failed' });
    // No room.join went out: it would have kept the MediaPeer whose page side is gone.
    expect(h.hub.joins).toEqual(['lounge', 'games']);

    h.server.handle('room.leave', () => ({ ok: {} }));
    await session().join('lounge');
    expect(sentOnSocket().slice(-2)).toEqual(['room.leave', 'room.join']);
    expect(room().joinState).toBe('joined');
  });

  it('switching rooms ends the old room’s media: the share, the sub PC, the subscriptions', async () => {
    await inRoom('lounge');
    const share = await startShare();
    h.server.send('pc.offer', subOffer(1));
    session().subscriptions.set(wants(2));
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(h.hub.subscribes).toHaveLength(1);

    await session().join('games');
    expect(share.calls).toEqual(['stop']);
    expect(session().share).toBeNull();
    expect(h.subscribers[0]?.closed).toBe(true);
    expect(session().subscriptions.desired).toEqual([]);
    expect(room()).toMatchObject({ roomId: 'games', joinState: 'joined' });
  });
});

describe('room.state', () => {
  beforeEach(async () => {
    await inRoom();
  });

  it('replaces the stored snapshot only when rev is newer', () => {
    const newer = h.hub.sendState('lounge', { participants: [participant('b8f2n4r6t0vz', 'Bea')] });
    expect(room().state).toEqual(newer);
    h.server.send('room.state', roomState('lounge', newer.rev, { participants: [] }));
    h.server.send('room.state', roomState('lounge', newer.rev - 1, { participants: [] }));
    expect(room().state).toEqual(newer);
  });

  it('ignores the snapshot of another room', () => {
    const before = room().state;
    h.server.send('room.state', roomState('games', 99));
    expect(room().state).toBe(before);
  });

  it('starts rev again with every welcome: a restarted server counts from low numbers', async () => {
    h.hub.rev = 500;
    h.hub.sendState('lounge');
    expect(room().state?.rev).toBe(500);
    h.hub.rev = 3;
    await reconnect({ resumed: false });
    expect(room().state?.rev).toBe(3);
  });

  it('has helpers for the snapshot', () => {
    const state = roomState('lounge', 9, {
      participants: [participant('b8f2n4r6t0vz', 'Bea')],
      shares: [shareInfo('s1', ownUser, 'c_me'), shareInfo('s2', 'b8f2n4r6t0vz', 'c_bea')],
    });
    expect(sharesOfConnection(state, 'c_me').map((s) => s.id)).toEqual(['s1']);
    expect(sharesOfConnection(null, 'c_me')).toEqual([]);
    expect(sharesOfConnection(state, null)).toEqual([]);
    expect(participantOf(state, 'b8f2n4r6t0vz')?.name).toBe('Bea');
    expect(participantOf(state, 'nobody')).toBeUndefined();
  });
});

describe('leave', () => {
  it('stops the share, sends room.leave, closes the sub PC and clears the desired room', async () => {
    await inRoom();
    const share = await startShare();
    let leavesWhenStopped = -1;
    const stop = share.stop.bind(share);
    share.stop = () => {
      leavesWhenStopped = h.hub.sent('room.leave').length;
      return stop();
    };
    h.server.send('pc.offer', subOffer(1));
    session().subscriptions.set(wants(1));

    await session().leave();
    // The share's own goodbye (share.stop, pc.close {pub}) goes out before room.leave.
    expect(leavesWhenStopped).toBe(0);
    expect(h.hub.sent('room.leave')).toHaveLength(1);
    expect(h.subscribers[0]?.closed).toBe(true);
    expect(session().share).toBeNull();
    expect(session().roomId).toBeNull();
    expect(session().subscriptions.desired).toEqual([]);
    expect(room()).toMatchObject({ roomId: null, joinState: 'idle', room: null, state: null });
  });

  it('resolves a join() that was still waiting', async () => {
    const joined = track(session().join('lounge'));
    await session().leave();
    expect(joined).toEqual({ done: true, value: undefined });
    expect(h.hub.sent('room.leave')).toEqual([]);
  });

  it('after leave(), a welcome joins nothing (no fallback to the last or the default room)', async () => {
    await inRoom();
    await session().leave();
    await reconnect();
    await reconnect({ resumed: false });
    expect(h.hub.joins).toEqual(['lounge']);
    expect(room()).toMatchObject({ roomId: null, joinState: 'idle' });
  });

  it('offline it only cleans up, and a resumed welcome that still has the room sends the room.leave', async () => {
    await inRoom();
    h.server.autoOpen = false;
    h.server.drop();
    await session().leave();
    expect(h.hub.sent('room.leave')).toEqual([]);
    expect(room().joinState).toBe('idle');

    h.server.autoOpen = true;
    h.server.welcomeDefaults = { roomId: 'lounge' };
    await tick(BackoffMaxMs * 2);
    expect(h.runtime.signal.state).toBe('ready');
    expect(h.hub.sent('room.leave')).toHaveLength(1);
    expect(h.hub.joins).toEqual(['lounge']);
  });

  it('a join() during leave() owns the room: no room.leave after its room.join', async () => {
    await inRoom();
    await startShare();
    const left = session().leave();
    const joined = session().join('games');
    await Promise.all([left, joined]);
    expect(h.hub.sent('room.leave')).toEqual([]);
    expect(room()).toMatchObject({ roomId: 'games', joinState: 'joined' });
  });

  it('a join() of the same room during leave(): one room.leave, before its room.join', async () => {
    await inRoom();
    await startShare();
    h.server.send('pc.offer', subOffer(2));
    const left = session().leave();
    const joined = session().join('lounge');
    await Promise.all([left, joined]);
    // leave() ended the room's media on the page, so the server's MediaPeer has to go too (01 §8.4).
    expect(sentOnSocket().slice(-2)).toEqual(['room.leave', 'room.join']);
    expect(h.hub.sent('room.leave')).toHaveLength(1);
    expect(room()).toMatchObject({ roomId: 'lounge', joinState: 'joined' });
    expect(h.subscribers[0]?.closed).toBe(true);
  });
});

describe('resync (01 §10.5)', () => {
  it('with no desired room, the first welcome falls back to isshoni.lastRoomId', async () => {
    h.services.prefs.getState().setLastRoomId('games');
    await h.connect();
    expect(h.hub.joins).toEqual(['games']);
    expect(room()).toMatchObject({ roomId: 'games', joinState: 'joined' });
  });

  it('…and then to welcome.defaultRoomId', async () => {
    await h.connect();
    expect(h.hub.joins).toEqual(['lounge']);
    expect(session().roomId).toBe('lounge');
  });

  describe('resumed', () => {
    it('into the wanted room: no room.join, the share resyncs, the full subscription set is re-sent', async () => {
      await inRoom();
      const share = await startShare();
      session().subscriptions.set(wants(70));
      await tick(SUBSCRIBE_DEBOUNCE_MS);
      expect(h.hub.subscribes.map((s) => s.length)).toEqual([64, 6]);

      await reconnect({ roomId: 'lounge' });
      expect(h.hub.joins).toEqual(['lounge']);
      expect(share.calls).toEqual(['resync:kept']);
      expect(share.resyncs[0]).toMatchObject({ roomId: 'lounge', kept: true, welcome: { resumed: true } });
      // The full desired set, in chunks of at most 64.
      expect(h.hub.subscribes.slice(2).map((s) => s.length)).toEqual([64, 6]);
      expect(h.hub.subscribes.slice(2).flat()).toEqual(wants(70));
      expect(room()).toMatchObject({ joinState: 'joined', connectionId: ownConnection() });
    });

    it('sends no subscribe.update when nothing is wanted', async () => {
      await inRoom();
      await reconnect({ roomId: 'lounge' });
      expect(h.hub.subscribes).toEqual([]);
    });

    it('turns off what the server still holds and the viewer stopped wanting while offline', async () => {
      await inRoom();
      session().subscriptions.set(wants(3));
      await tick(SUBSCRIBE_DEBOUNCE_MS);
      h.server.autoOpen = false;
      h.server.drop();
      session().subscriptions.set(wants(2));
      h.server.autoOpen = true;
      h.server.welcomeDefaults = { roomId: 'lounge' };
      await tick(BackoffMaxMs * 2);
      expect(h.hub.subscribes.at(-1)).toEqual([...wants(2), { shareId: 's2', video: 'off', audio: 'off' }]);
    });

    it('keeps the sub PC, whose server side lived through the grace period', async () => {
      await inRoom();
      h.server.send('pc.offer', subOffer(1));
      await reconnect({ roomId: 'lounge' });
      h.server.send('pc.offer', subOffer(1, 2));
      expect(h.subscribers).toHaveLength(1);
      expect(h.subscribers[0]).toMatchObject({ closed: false, offers: [subOffer(1), subOffer(1, 2)] });
    });

    it('resolves a join() made while the connection was down', async () => {
      await inRoom();
      h.server.autoOpen = false;
      h.server.drop();
      // The room page mounts again while offline: same room, already joined.
      await session().join('lounge');
      h.server.autoOpen = true;
      h.server.welcomeDefaults = { roomId: 'lounge' };
      await tick(BackoffMaxMs * 2);
      expect(h.hub.joins).toEqual(['lounge']);
      expect(room().joinState).toBe('joined');
    });

    it.each([
      ['a missing roomId', undefined],
      ['another roomId', 'games'],
    ])('with %s: joins the desired room first, and the media starts over', async (_name, roomId) => {
      await inRoom();
      const share = await startShare();
      h.server.send('pc.offer', subOffer(4));
      session().subscriptions.set(wants(2));
      await tick(SUBSCRIBE_DEBOUNCE_MS);

      await reconnect(roomId !== undefined ? { roomId } : {});
      expect(h.hub.joins).toEqual(['lounge', 'lounge']);
      expect(room()).toMatchObject({ roomId: 'lounge', joinState: 'joined' });
      // Joining gave the connection a new MediaPeer (01 §8.4): nothing of the old one is left on the server.
      expect(share.calls).toEqual(['resync:lost']);
      expect(h.subscribers[0]?.closed).toBe(true);
      expect(h.hub.subscribes.at(-1)).toEqual(wants(2));
      // The join comes before the media.
      const types = h.server.messages(undefined, h.server.socket).map((m) => m.type);
      expect(types.indexOf('room.join')).toBeLessThan(types.indexOf('subscribe.update'));
    });

    it.each<[string, () => Promise<void>]>([
      [
        'leave() and the same room again',
        async () => {
          await session().leave();
          void session().join('lounge');
        },
      ],
      [
        'a switch to another room and back',
        () => {
          void session().join('games');
          void session().join('lounge');
          return Promise.resolve();
        },
      ],
    ])('after %s while offline: room.leave, then room.join, and the media starts over', async (_name, away) => {
      await inRoom();
      const share = await startShare();
      h.server.send('pc.offer', subOffer(4));
      session().subscriptions.set(wants(3));
      await tick(SUBSCRIBE_DEBOUNCE_MS);

      await reconnectToHello();
      await away();
      // The page ended the room's media: the share, the sub PC, the subscriptions.
      expect(share.calls).toEqual(['stop']);
      expect(h.subscribers[0]?.closed).toBe(true);
      session().subscriptions.set(wants(2));
      expect(h.hub.sent('room.leave')).toEqual([]);

      // The server kept the connection in lounge through the grace period, with the MediaPeer of before: its sub PC
      // at gen 4 and the three subscriptions (01 §10.3). Its Resync() follows the welcome (01 §10.5).
      const w = h.server.welcome({ roomId: 'lounge' });
      expect(w.resumed).toBe(true);
      h.server.send('pc.offer', subOffer(4, 2));
      await tick();

      // Joining the room it is already in would only answer ok and keep that MediaPeer (01 §8.4).
      expect(sentOnSocket()).toEqual(['room.leave', 'room.join', 'subscribe.update']);
      expect(room()).toMatchObject({ roomId: 'lounge', joinState: 'joined' });
      // The new MediaPeer holds nothing: only what is wanted now goes out, with no off for the third share.
      expect(h.hub.subscribes.at(-1)).toEqual(wants(2));
      // The old MediaPeer's offer made no sub PC; the new one's first offer does.
      expect(h.subscribers).toHaveLength(1);
      h.server.send('pc.offer', subOffer(1));
      expect(h.subscribers[1]?.offers).toEqual([subOffer(1)]);
      // Not a resumed room: the snapshot after the join is not searched for strays (the room.leave ended them).
      h.hub.sendState('lounge', { shares: [shareInfo('s_local', ownUser, ownConnection())] });
      expect(h.hub.sent('share.stop')).toEqual([]);
    });

    it('a switch to another room while offline needs no room.leave: only the room.join', async () => {
      await inRoom();
      h.server.send('pc.offer', subOffer(4));
      await reconnectToHello();
      void session().join('games');
      h.server.welcome({ roomId: 'lounge' });
      await tick();
      // Joining another room closes lounge's MediaPeer by itself (01 §8.4).
      expect(sentOnSocket()).toEqual(['room.join']);
      expect(room()).toMatchObject({ roomId: 'games', joinState: 'joined' });
    });

    it('stops a server share of this connection that the page no longer has', async () => {
      await inRoom();
      await startShare(new FakeShare('s_local'));
      h.hub.shares = [
        shareInfo('s_local', ownUser, ownConnection()),
        shareInfo('s_stray', ownUser, ownConnection()),
        shareInfo('s_other_tab', ownUser, 'c_another_tab'),
        shareInfo('s_bea', 'b8f2n4r6t0vz', 'c_bea'),
      ];
      await reconnect({ roomId: 'lounge' });
      expect(h.hub.sent('share.stop')).toEqual([]);
      h.hub.sendState('lounge'); // the room.state that follows a resumed welcome
      expect(h.hub.sent('share.stop').map((m) => m.data)).toEqual([{ shareId: 's_stray' }]);
      // Once per welcome: later snapshots are not searched for strays again.
      h.hub.sendState('lounge');
      expect(h.hub.sent('share.stop')).toHaveLength(1);
    });

    it('publishes a local share again when the server no longer has it', async () => {
      await inRoom();
      const share = await startShare();
      h.hub.shares = [];
      await reconnect({ roomId: 'lounge' });
      h.hub.sendState('lounge');
      await tick();
      expect(share.calls).toEqual(['resync:kept', 'republish']);
      expect(session().share).toBe(share);
    });

    it('compares with the snapshot that came before the share finished its own resync', async () => {
      await inRoom();
      const share = await startShare();
      let finish: () => void = () => undefined;
      share.resync = (ctx) => {
        share.calls.push(ctx.kept ? 'resync:kept' : 'resync:lost');
        return new Promise<void>((resolve) => {
          finish = resolve;
        });
      };
      h.hub.shares = [];
      await reconnect({ roomId: 'lounge' });
      h.hub.sendState('lounge'); // arrives while the pub PC is still restarting ICE
      await tick();
      expect(share.calls).toEqual(['resync:kept']);
      finish();
      await tick();
      expect(share.calls).toEqual(['resync:kept', 'republish']);
    });

    it('a share that cannot be published again is stopped instead', async () => {
      await inRoom();
      const share = await startShare(bareShare('s_local'));
      h.hub.shares = [];
      await reconnect({ roomId: 'lounge' });
      expect(share.calls).toEqual([]); // kept: nothing to do without resync()
      h.hub.sendState('lounge');
      await tick();
      expect(share.calls).toEqual(['stop']);
      expect(session().share).toBeNull();
    });
  });

  describe('not resumed', () => {
    it('discards the sub PC, joins the desired room, re-publishes the share, re-sends the subscriptions', async () => {
      await inRoom();
      const share = await startShare();
      h.server.send('pc.offer', subOffer(7));
      session().subscriptions.set(wants(3));
      await tick(SUBSCRIBE_DEBOUNCE_MS);

      // The restarted server knows none of the old share ids.
      h.hub.ignored = new Set(['s0', 's1', 's2']);
      await reconnect({ resumed: false });
      expect(h.hub.joins).toEqual(['lounge', 'lounge']);
      expect(room()).toMatchObject({ joinState: 'joined', connectionId: ownConnection() });
      expect(share.calls).toEqual(['resync:lost']);
      expect(share.resyncs[0]).toMatchObject({ kept: false, welcome: { resumed: false } });
      expect(h.subscribers[0]?.closed).toBe(true);
      expect(h.hub.subscribes.at(-1)).toEqual(wants(3));
      // The ignored ids are gone; the viewer maps its wants to the new share ids from the new room.state.
      expect(session().subscriptions.desired).toEqual([]);

      // A new sub PC for the new connection: gen starts again.
      h.server.send('pc.offer', subOffer(1));
      expect(h.subscribers).toHaveLength(2);
      expect(h.subscribers[1]?.offers).toEqual([subOffer(1)]);
    });

    it('sends no subscribe.update when nothing is wanted', async () => {
      await inRoom();
      await reconnect({ resumed: false });
      expect(h.hub.subscribes).toEqual([]);
    });

    it('re-sends only what is wanted: the new connection holds nothing to turn off', async () => {
      await inRoom();
      session().subscriptions.set(wants(3));
      await tick(SUBSCRIBE_DEBOUNCE_MS);
      h.server.autoOpen = false;
      h.server.restart();
      h.server.drop();
      session().subscriptions.set(wants(2));
      h.server.autoOpen = true;
      await tick(BackoffMaxMs * 2);
      expect(h.server.welcomes.at(-1)?.resumed).toBe(false);
      expect(h.hub.subscribes.at(-1)).toEqual(wants(2));
    });

    it('a share without resync() is stopped: the server lost it and it cannot be published again', async () => {
      await inRoom();
      const share = await startShare(bareShare('s_local'));
      await reconnect({ resumed: false });
      expect(share.calls).toEqual(['stop']);
      expect(session().share).toBeNull();
    });

    it('does not look for strays or missing shares: the new connection has neither', async () => {
      await inRoom();
      const share = await startShare();
      h.hub.shares = [shareInfo('s_stray', ownUser, 'c_old_connection')];
      await reconnect({ resumed: false });
      h.hub.sendState('lounge');
      await tick();
      expect(h.hub.sent('share.stop')).toEqual([]);
      expect(share.calls).toEqual(['resync:lost']);
    });

    it('a room that is refused now fails the join; the next welcome tries again', async () => {
      await inRoom();
      h.server.handle('room.join', () => ({ error: makeError('room_full', 'request') }));
      await reconnect({ resumed: false });
      expect(room()).toMatchObject({ roomId: 'lounge', joinState: 'failed' });
      h.server.handle('room.join', (data) => ({ ok: { room: { id: data.roomId, name: 'Lounge' } } }));
      await reconnect({ resumed: false });
      expect(room().joinState).toBe('joined');
    });
  });
});

describe('subscriptions', () => {
  it('not_in_room: rejoins the desired room and sends the full set', async () => {
    await inRoom();
    let calls = 0;
    h.server.handle('subscribe.update', () =>
      calls++ === 0 ? { error: makeError('not_in_room', 'request') } : { ok: { ignored: [] } },
    );
    session().subscriptions.set(wants(2));
    await tick(SUBSCRIBE_DEBOUNCE_MS);
    expect(h.hub.joins).toEqual(['lounge', 'lounge']);
    expect(h.hub.subscribes).toEqual([wants(2), wants(2)]);
    expect(room().joinState).toBe('joined');
  });

  it('a server that keeps answering not_in_room gets one rejoin and one more try, not a loop (05 §6.3)', async () => {
    await inRoom();
    h.server.handle('subscribe.update', () => ({ error: makeError('not_in_room', 'request') }));
    session().subscriptions.set(wants(2));
    await tick(60_000);
    expect(h.hub.subscribes).toEqual([wants(2), wants(2)]);
    expect(h.hub.joins).toEqual(['lounge', 'lounge']);
    expect(room()).toMatchObject({ roomId: 'lounge', joinState: 'joined' });
  });
});

describe('room.event announcements (05 §11.1)', () => {
  beforeEach(async () => {
    await inRoom();
  });

  it.each<[string, Partial<RoomEvent>, string]>([
    ['participant.joined', {}, 'Bea joined'],
    ['participant.left', { reason: 'left' }, 'Bea left'],
    ['participant.left', { reason: 'disconnected' }, 'Bea disconnected'],
    ['share.started', { shareId: 's1' }, 'Bea started sharing'],
    ['share.stopped', { shareId: 's1', reason: 'stopped' }, 'Bea stopped sharing'],
    ['share.stopped', { shareId: 's1', reason: 'left' }, 'Bea stopped sharing'],
    ['share.stopped', { shareId: 's1', reason: 'media_timeout' }, "Bea's share ended"],
    ['share.stopped', { shareId: 's1', reason: 'disconnected' }, "Bea's share ended"],
  ])('%s %j → "%s"', (kind, overrides, text) => {
    h.server.send('room.event', event(kind, overrides));
    expect(h.toasts()).toEqual([text]);
    // A toast is also read by the polite live region (05 §16.6).
    expect(h.services.ui.getState().announcements.polite?.text).toBe(text);
  });

  it('uses the name in the event, also for someone who is not in the snapshot anymore', () => {
    h.server.send('room.event', event('participant.left', { userId: 'gone', name: 'Cy', reason: 'left' }));
    expect(h.toasts()).toEqual(['Cy left']);
  });

  it('does not announce the user’s own events', () => {
    for (const kind of ['participant.joined', 'participant.left', 'share.started', 'share.stopped']) {
      h.server.send('room.event', event(kind, { userId: ownUser, name: 'Alex', shareId: 's9' }));
    }
    expect(h.toasts()).toEqual([]);
  });

  it('does not announce the share.started of a re-publish', () => {
    h.server.send('room.event', event('share.started', { shareId: 's2', replaces: 's1' }));
    expect(h.toasts()).toEqual([]);
  });

  it('ignores events of another room and kinds this build does not know', () => {
    h.server.send('room.event', event('participant.joined', { roomId: 'games' }));
    h.server.send('room.event', event('participant.waved'));
    expect(h.toasts()).toEqual([]);
  });

  it('lets a tap take an announcement over, and shows the others', () => {
    const seen: string[] = [];
    const off = session().onRoomEvent((e) => {
      seen.push(e.kind);
      return e.kind === 'share.started'; // the viewer shows "Bea started sharing [Watch]" itself
    });
    h.server.send('room.event', event('share.started', { shareId: 's1' }));
    h.server.send('room.event', event('participant.joined'));
    expect(seen).toEqual(['share.started', 'participant.joined']);
    expect(h.toasts()).toEqual(['Bea joined']);
    off();
    h.server.send('room.event', event('share.started', { shareId: 's2' }));
    expect(h.toasts()).toEqual(['Bea joined', 'Bea started sharing']);
  });

  it('a tap that throws does not stop the announcement', () => {
    session().onRoomEvent(() => {
      throw new Error('boom');
    });
    h.server.send('room.event', event('participant.joined'));
    expect(h.toasts()).toEqual(['Bea joined']);
  });
});

describe('the local share', () => {
  beforeEach(async () => {
    await inRoom();
  });

  it('starts through platform.sharing with the session’s signal and room', async () => {
    const changes: (string | null)[] = [];
    session().on('share', (s) => changes.push(s?.shareId ?? null));
    const share = await startShare();
    expect(h.sharing.starts).toEqual([
      { opts: { preset: 'auto', withAudio: true }, ctx: { signal: h.runtime.signal, roomId: 'lounge' } },
    ]);
    expect(changes).toEqual(['s_local']);
    // It ends by itself (the browser's "Stop sharing").
    share.end('browser-stopped');
    expect(session().share).toBeNull();
    expect(changes).toEqual(['s_local', null]);
  });

  it('rejects where sharing is not possible, before the room is joined, and for a second share', async () => {
    const opts = { preset: 'auto', withAudio: false } as const;
    await startShare();
    await expect(session().startShare(pickedSource, opts)).rejects.toThrow(/already shares/);
    await session().leave();
    await expect(session().startShare(pickedSource, opts)).rejects.toMatchObject({ code: 'not_ready', local: true });

    h.close();
    h = createHarness({ platform: { sharing: null } });
    await inRoom();
    await expect(session().startShare(pickedSource, opts)).rejects.toBeInstanceOf(LocalError);
  });

  it('passes the provider’s error on', async () => {
    const limit = ProtocolError.fromWire(makeError('share_limit', 'request', { params: { limit: 4, per: 'user' } }));
    h.sharing.next.push(limit);
    await expect(session().startShare(pickedSource, { preset: 'auto', withAudio: true })).rejects.toBe(limit);
    expect(session().share).toBeNull();
    // And it can try again.
    await startShare();
  });

  it('not_in_room: rejoins the desired room and tries once more', async () => {
    h.sharing.next.push(ProtocolError.fromWire(makeError('not_in_room', 'request')));
    const share = await startShare();
    expect(h.hub.joins).toEqual(['lounge', 'lounge']);
    expect(h.sharing.starts).toHaveLength(2);
    expect(session().share).toBe(share);
  });

  it('stopShare() stops it; its own share.stopped is not "the server ended it"', async () => {
    const share = await startShare();
    const stop = share.stop.bind(share);
    share.stop = async () => {
      // The server's event arrives while the stop is still on its way.
      h.server.send('room.event', event('share.stopped', { userId: ownUser, shareId: 's_local', reason: 'stopped' }));
      h.hub.sendState('lounge', { shares: [] });
      await stop();
    };
    await session().stopShare();
    expect(share.calls).toEqual(['stop']);
    expect(session().share).toBeNull();
    expect(h.toasts()).toEqual([]);
  });

  it.each<EndReason>(['stopped', 'left', 'media_timeout', 'room_closed', 'disconnected', 'kicked', 'server_shutdown'])(
    'own share.stopped with reason %s drives the share’s state machine, without a toast (05 §13.1)',
    async (reason) => {
      const share = await startShare();
      h.server.send(
        'room.event',
        event('share.stopped', { userId: ownUser, name: 'Alex', shareId: 's_local', reason }),
      );
      expect(share.calls).toEqual([`serverEnded:${reason}`]);
      expect(session().share).toBeNull();
      expect(h.toasts()).toEqual([]);
      // The snapshot without it that follows is not a second ending.
      h.hub.sendState('lounge', { shares: [] });
      expect(share.calls).toHaveLength(1);
    },
  );

  it('the share.stopped of another share of this user (another tab) is not this tab’s share', async () => {
    const share = await startShare();
    h.server.send('room.event', event('share.stopped', { userId: ownUser, shareId: 's_other_tab', reason: 'stopped' }));
    expect(share.calls).toEqual([]);
    expect(session().share).toBe(share);
  });

  it('fallback: a newer room.state without the share, once a snapshot had listed it', async () => {
    const share = await startShare();
    // Not yet listed: a snapshot from before share.start reached the hub proves nothing.
    h.hub.sendState('lounge', { shares: [] });
    expect(share.calls).toEqual([]);
    h.hub.sendState('lounge', { shares: [shareInfo('s_local', ownUser, ownConnection(), { status: 'starting' })] });
    h.hub.sendState('lounge', { shares: [] });
    expect(share.calls).toEqual(['serverEnded:room.state']);
    expect(session().share).toBeNull();
  });

  it('the fallback does not run while a resync runs', async () => {
    const share = await startShare();
    h.hub.sendState('lounge', { shares: [shareInfo('s_local', ownUser, ownConnection())] });
    let finish: () => void = () => undefined;
    share.resync = () =>
      new Promise<void>((resolve) => {
        finish = resolve;
      });
    // A new connection: the new server's first snapshots don't have the share, which is being re-published.
    await reconnect({ resumed: false });
    h.hub.sendState('lounge', { shares: [] });
    expect(share.calls).toEqual([]);
    expect(session().share).toBe(share);
    finish();
  });

  it('a share without serverEnded() is stopped instead', async () => {
    const share = await startShare(bareShare('s_local'));
    h.server.send(
      'room.event',
      event('share.stopped', { userId: ownUser, shareId: 's_local', reason: 'media_timeout' }),
    );
    expect(share.calls).toEqual(['stop']);
    expect(session().share).toBeNull();
  });
});

describe('room-scope errors (05 §6.3)', () => {
  beforeEach(async () => {
    await inRoom('games');
  });

  it('room_closed: leaves the room UI, tells the user, and sends the UI to the default room', async () => {
    const share = await startShare();
    h.server.send('pc.offer', subOffer(1));
    h.server.error(makeError('room_closed', 'room', { roomId: 'games' }));
    expect(h.toasts()).toEqual(['An admin deleted this room.']);
    expect(room()).toMatchObject({
      roomId: null,
      joinState: 'idle',
      state: null,
      redirect: { roomId: 'lounge', code: 'room_closed' },
    });
    expect(session().roomId).toBeNull();
    expect(share.calls).toEqual(['stop']);
    expect(h.subscribers[0]?.closed).toBe(true);
    // The deleted room is not the one to come back to.
    expect(h.platform.storage.local.get('isshoni.lastRoomId')).toBeNull();
    // No room.leave: the server already took the connection out.
    expect(h.hub.sent('room.leave')).toEqual([]);
    // And no welcome brings it back.
    await reconnect();
    expect(h.hub.joins).toEqual(['games']);
  });

  it.each([
    ['kicked', 'You were removed from the room.'],
    ['room_archived', 'Something went wrong (room_archived).'],
  ])('%s (reserved or unknown): the same, by scope (01 §12.3)', (code, text) => {
    h.server.error(makeError(code, 'room'));
    expect(h.toasts()).toEqual([text]);
    expect(room().redirect).toEqual({ roomId: 'lounge', code });
  });

  it('ignores an error about a room the session already left, and errors of other scopes', () => {
    h.server.error(makeError('room_closed', 'room', { roomId: 'old' }));
    h.server.error(makeError('sdp_invalid', 'pc', { pc: 'sub', gen: 1, neg: 1 }));
    h.server.error(makeError('codec_not_supported', 'share', { shareId: 's1' }));
    expect(room()).toMatchObject({ roomId: 'games', joinState: 'joined', redirect: null });
    expect(h.toasts()).toEqual([]);
  });

  it('in the default room itself there is nowhere to go: a failed join', async () => {
    await session().join('lounge');
    h.server.error(makeError('room_closed', 'room', { roomId: 'lounge' }));
    expect(room()).toMatchObject({ roomId: 'lounge', joinState: 'failed', redirect: null, state: null });
    expect(room().joinError?.code).toBe('room_closed');
  });
});

describe('the sub PC', () => {
  it('is made on the first sub offer and gets the sub offers and candidates, not the pub ones', async () => {
    await inRoom();
    expect(h.subscribers).toEqual([]);
    const ice = { pc: 'sub', gen: 1, candidate: { candidate: 'candidate:1 1 udp 1 192.0.2.1 7882 typ host' } } as const;
    h.server.send('pc.offer', subOffer(1));
    h.server.send('pc.ice', ice);
    h.server.send('pc.ice', { pc: 'pub', gen: 1 });
    h.server.send('pc.offer', subOffer(1, 2));
    expect(h.subscribers).toHaveLength(1);
    expect(h.subscribers[0]?.offers).toEqual([subOffer(1), subOffer(1, 2)]);
    expect(h.subscribers[0]?.candidates).toEqual([ice]);
    // It is made with what SubscriberPC needs besides its registry (05 §10.1).
    expect(h.subscribers[0]?.deps).toMatchObject({ platform: h.platform, signal: h.runtime.signal });
  });

  it('gets nothing while a room.join is on its way: those are the old room’s messages (01 §8.4)', async () => {
    await inRoom();
    h.server.send('pc.offer', subOffer(1));
    const joined = session().join('games');
    // lounge's MediaPeer was still negotiating when the switch came: its offer and a candidate are ahead of the ok.
    h.server.send('pc.offer', subOffer(1, 2));
    h.server.send('pc.ice', { pc: 'sub', gen: 1 });
    expect(h.subscribers).toHaveLength(1);
    await joined;
    expect(h.subscribers).toHaveLength(1);

    // games' MediaPeer starts over at gen 1, neg 1, in a sub PC of its own.
    h.server.send('pc.offer', subOffer(1));
    expect(h.subscribers).toHaveLength(2);
    expect(h.subscribers[0]).toMatchObject({ closed: true, offers: [subOffer(1)], candidates: [] });
    expect(h.subscribers[1]).toMatchObject({ closed: false, offers: [subOffer(1)], candidates: [] });
  });

  it('gets nothing after a resumed welcome that still has the room the user switched away from', async () => {
    await inRoom();
    h.server.send('pc.offer', subOffer(3));
    await reconnectToHello();
    void session().join('games');
    // The server's Resync() ICE-restarts lounge's sub PC right behind the welcome, before it reads the room.join
    // (01 §10.5).
    h.server.welcome({ roomId: 'lounge' });
    h.server.send('pc.offer', subOffer(3, 2));
    expect(h.subscribers).toHaveLength(1);
    await tick();
    expect(room()).toMatchObject({ roomId: 'games', joinState: 'joined' });
    h.server.send('pc.offer', subOffer(1));
    expect(h.subscribers).toHaveLength(2);
    expect(h.subscribers[1]?.offers).toEqual([subOffer(1)]);
  });

  it('is not made from a late offer when the join is refused', async () => {
    await inRoom();
    h.server.send('pc.offer', subOffer(1));
    h.server.handle('room.join', () => ({ error: makeError('room_full', 'request') }));
    const joined = track(session().join('games'));
    h.server.send('pc.offer', subOffer(1, 2)); // ahead of the refusal
    await tick();
    expect((joined.error as ProtocolError).code).toBe('room_full');
    h.server.send('pc.offer', subOffer(1, 3)); // and behind it
    expect(h.subscribers).toHaveLength(1);
    expect(h.subscribers[0]).toMatchObject({ closed: true, offers: [subOffer(1)] });
  });

  it('is not made without a room, and the session runs without a viewer', async () => {
    await h.connect();
    await session().leave();
    h.server.send('pc.offer', subOffer(1));
    expect(h.subscribers).toEqual([]);

    h.close();
    h = createHarness({ runtime: { media: {} } });
    await inRoom();
    h.server.send('pc.offer', subOffer(1));
    expect(h.subscribers).toEqual([]);
  });
});

describe('dispose', () => {
  it('drops the listeners, closes the sub PC, stops the share and settles waiting joins', async () => {
    await inRoom();
    const share = await startShare();
    h.server.send('pc.offer', subOffer(1));
    session().dispose();
    expect(share.calls).toEqual(['stop']);
    expect(h.subscribers[0]?.closed).toBe(true);
    const before = room().state;
    h.hub.sendState('lounge');
    h.server.send('room.event', event('participant.joined'));
    expect(room().state).toBe(before);
    expect(h.toasts()).toEqual([]);
    await expect(session().join('lounge')).rejects.toThrow(/dispose/);
    // A welcome after it does nothing.
    await reconnect({ resumed: false });
    expect(h.hub.joins).toEqual(['lounge']);
  });
});
