// The desktop app's native share in the room runtime (docs/m2/03 §9, §10.2, §10.4; README WA65): the share that C2
// publishes outlives the page, so its room becomes the page's room (RoomSession.followShareRoom) and the runtime
// adopts it once a room.state lists it (RoomSession.adoptShare), also when the provider learns of it after the
// snapshot (ready, onChange); the session then shows and stops it like one the page started, its end is C2's to
// report (serverEnded only refreshes), and the viewer counts C2's shares as the page's own
// (platform.sharing.localConnectionIds()).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { ActiveShare, PickedSource, SharingProvider } from '../platform/types';
import type { RoomEvent } from '../protocol/types.gen';
import { createHarness, FakeShare, shareInfo, tick, type Harness } from './testing/harness';

/** A native SharingProvider whose current share, local connections and readiness the test sets. */
class NativeSharing implements SharingProvider {
  readonly mode = 'native' as const;
  share: ActiveShare | null = null;
  ids: string[] = ['c_publisher'];
  readonly stopped: [string, string][] = [];
  readonly #listeners = new Set<() => void>();
  #ready: () => void = () => undefined;
  readonly #readyPromise = new Promise<void>((resolve) => {
    this.#ready = resolve;
  });

  /** The provider has said what it publishes (DesktopSharing's first Share.Current() answer). */
  becomeReady(): void {
    this.#ready();
  }
  /** current() or localConnectionIds() answer differently now. */
  changed(): void {
    for (const fn of [...this.#listeners]) fn();
  }
  ready(): Promise<void> {
    return this.#readyPromise;
  }
  onChange(fn: () => void): () => void {
    this.#listeners.add(fn);
    return () => {
      this.#listeners.delete(fn);
    };
  }

  pick(): Promise<PickedSource | null> {
    return Promise.resolve(null);
  }
  start(): Promise<ActiveShare> {
    return Promise.reject(new Error('not in this test'));
  }
  current(): ActiveShare | null {
    return this.share;
  }
  localConnectionIds(): string[] {
    return this.ids;
  }
  stopOther(roomId: string, shareId: string): Promise<void> {
    this.stopped.push([roomId, shareId]);
    return Promise.resolve();
  }
}

const ownUser = 'k3m9p2qxw7ht'; // the fixtures' welcome.user.id

let h: Harness;
let native: NativeSharing;

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(Math, 'random').mockReturnValue(0.5);
  native = new NativeSharing();
  h = createHarness({ platform: { sharing: native } });
});

afterEach(() => {
  h.close();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

const session = () => h.runtime.session;

async function inRoom(roomId = 'lounge'): Promise<void> {
  const joined = session().join(roomId);
  await h.connect();
  await joined;
}

const nativeInfo = (id = 's_native') => shareInfo(id, ownUser, 'c_publisher');

/** The app's share as DesktopShare names it: with the room C2 publishes it in. */
const shareIn = (roomId: string, id = 's_native') => Object.assign(new FakeShare(id), { roomId });

const room = () => h.runtime.stores.room.getState();

describe('the runtime adopts the app’s share (docs/m2/03 §9)', () => {
  it('once a room.state of the session’s room lists the share platform.sharing.current() names', async () => {
    const share = new FakeShare('s_native');
    native.share = share;
    const changes: (string | null)[] = [];
    session().on('share', (s) => changes.push(s?.shareId ?? null));
    await inRoom();
    expect(session().share).toBeNull();
    h.hub.sendState('lounge', { shares: [nativeInfo()] });
    expect(session().share).toBe(share);
    expect(changes).toEqual(['s_native']);
    // A later snapshot changes nothing.
    h.hub.sendState('lounge', { shares: [nativeInfo()] });
    expect(changes).toEqual(['s_native']);
  });

  it('not a share the snapshot doesn’t list, one that ended, or one without an id yet', async () => {
    await inRoom();
    native.share = new FakeShare('s_native');
    h.hub.sendState('lounge', { shares: [] });
    expect(session().share).toBeNull();
    const ended = new FakeShare('s_native');
    ended.state = 'ended';
    native.share = ended;
    h.hub.sendState('lounge', { shares: [nativeInfo()] });
    expect(session().share).toBeNull();
    native.share = new FakeShare('');
    h.hub.sendState('lounge', { shares: [nativeInfo()] });
    expect(session().share).toBeNull();
  });

  it('adoptShare refuses while the session has a share, outside a room, and an ended share', async () => {
    const share = new FakeShare('s_native');
    expect(session().adoptShare(share)).toBe(false); // no room yet
    await inRoom();
    const ended = new FakeShare('s_old');
    ended.state = 'ended';
    expect(session().adoptShare(ended)).toBe(false);
    expect(session().adoptShare(share)).toBe(true);
    expect(session().adoptShare(new FakeShare('s_second'))).toBe(false);
    expect(session().share).toBe(share);
  });

  it('an adopted share stops like one the page started, and leaves when it ends', async () => {
    const share = new FakeShare('s_native');
    native.share = share;
    await inRoom();
    h.hub.sendState('lounge', { shares: [nativeInfo()] });
    await session().stopShare();
    expect(share.calls).toEqual(['stop']);
    expect(session().share).toBeNull();
  });

  it('leaving the room stops it (a share follows the page’s room)', async () => {
    const share = new FakeShare('s_native');
    native.share = share;
    await inRoom();
    h.hub.sendState('lounge', { shares: [nativeInfo()] });
    await session().leave();
    expect(share.calls).toEqual(['stop']);
  });
});

describe('the share’s room is the page’s room (docs/m2/03 §9)', () => {
  it('a reload that lands in another room: the page is sent to the share’s room, which adopts it', async () => {
    const share = shareIn('games');
    native.share = share;
    await inRoom('lounge'); // the route's room
    await tick();
    expect(room().redirect).toEqual({ roomId: 'games', code: 'share' });
    expect(session().share).toBeNull();
    // The room page follows the redirect (useRoomSession): it clears it, and the new route joins.
    room().clearRedirect();
    const joined = session().join('games');
    await tick();
    await joined;
    h.hub.sendState('games', { shares: [nativeInfo()] });
    expect(session().share).toBe(share);
    expect(room().redirect).toBeNull();
  });

  it('once per share: going elsewhere before it was adopted isn’t sent back', async () => {
    native.share = shareIn('games');
    await inRoom('lounge');
    await tick();
    expect(room().redirect).toMatchObject({ roomId: 'games' });
    room().clearRedirect();
    void session().join('games');
    await tick();
    void session().join('lounge');
    await tick();
    expect(room().redirect).toBeNull();
  });

  it('checked when the provider is ready: the route’s room is another, so the page goes to the share’s', async () => {
    await inRoom('lounge');
    native.share = shareIn('games');
    await tick();
    expect(room().redirect).toBeNull();
    native.becomeReady();
    await tick();
    expect(room().redirect).toEqual({ roomId: 'games', code: 'share' });
  });

  it('never for a share in the page’s room, nor for one the page held: a confirmed switch stops it', async () => {
    // A share whose end comes later, as C2's final view does after Share.Stop.
    class StoppingShare extends FakeShare {
      readonly roomId = 'lounge';
      override stop(): Promise<void> {
        this.calls.push('stop');
        return Promise.resolve();
      }
    }
    const own = new StoppingShare('s_native');
    native.share = own;
    await inRoom('lounge');
    native.becomeReady();
    await tick();
    expect(room().redirect).toBeNull();
    h.hub.sendState('lounge', { shares: [nativeInfo()] });
    expect(session().share).toBe(own);
    expect(session().followShareRoom('games')).toBe(false);
    // "Stop sharing and switch?" yes: the session stops it and joins; Go still reports it until its end arrives.
    const joined = session().join('games');
    await tick();
    await joined;
    expect(own.calls).toEqual(['stop']);
    expect(native.current()).toBe(own);
    native.changed();
    await tick();
    expect(room().redirect).toBeNull();
    expect(room().roomId).toBe('games');
  });
});

describe('a share the provider learns of after the snapshot (docs/m2/03 §9, §10.4)', () => {
  it('is adopted and marked local without a new room.state', async () => {
    native.ids = [];
    await inRoom();
    h.hub.sendState('lounge', { shares: [nativeInfo()] });
    expect(session().share).toBeNull();
    expect(h.runtime.viewer.store.getState().shares[0]).toMatchObject({ local: false });
    const share = shareIn('lounge');
    native.share = share;
    native.ids = ['c_publisher'];
    const snapshot = room().state;
    native.changed();
    expect(session().share).toBe(share);
    await tick();
    expect(h.runtime.viewer.store.getState().shares[0]).toMatchObject({ local: true, localPreview: false });
    expect(room().state).toBe(snapshot);
  });

  it('C2’s id alone, known late, marks its share local', async () => {
    native.ids = [];
    await inRoom();
    h.hub.sendState('lounge', { shares: [nativeInfo()] });
    expect(h.runtime.viewer.store.getState().shares[0]).toMatchObject({ local: false });
    native.ids = ['c_publisher'];
    native.changed();
    expect(h.runtime.viewer.store.getState().shares[0]).toMatchObject({ local: true });
  });
});

describe('the end is C2’s to report (docs/m2/03 §10.2)', () => {
  it('a share.stopped seen by the page only refreshes the share; its own end detaches it', async () => {
    const share = new FakeShare('s_native');
    native.share = share;
    await inRoom();
    h.hub.sendState('lounge', { shares: [nativeInfo()] });
    const stopped = {
      kind: 'share.stopped',
      roomId: 'lounge',
      userId: ownUser,
      name: 'Alex',
      shareId: 's_native',
      reason: 'stopped',
      at: '2026-10-12T19:41:07.500Z',
    } as RoomEvent;
    h.server.send('room.event', stopped);
    expect(share.calls).toEqual(['serverEnded:stopped']);
    expect(session().share).toBe(share);
    // A snapshot without it (C2 publishing again with replaces, or the end) is no second ending meanwhile.
    h.hub.sendState('lounge', { shares: [] });
    expect(share.calls).toEqual(['serverEnded:stopped']);
    share.end('server');
    expect(session().share).toBeNull();
  });
});

describe('the viewer counts C2’s shares as the page’s own (docs/m2/03 §10.4)', () => {
  it('marks them local, without a preview of the page', async () => {
    await inRoom();
    h.hub.sendState('lounge', { shares: [nativeInfo()] });
    expect(h.runtime.viewer.store.getState().shares).toEqual([
      expect.objectContaining({ id: 's_native', own: true, local: true, localPreview: false }),
    ]);
  });

  it('re-reads the snapshot when the local share changes, for ids that became known after it', async () => {
    native.ids = [];
    await inRoom();
    h.hub.sendState('lounge', { shares: [nativeInfo()] });
    expect(h.runtime.viewer.store.getState().shares[0]).toMatchObject({ local: false });
    native.ids = ['c_publisher'];
    const share = new FakeShare('s_native');
    expect(session().adoptShare(share)).toBe(true);
    await tick();
    expect(h.runtime.viewer.store.getState().shares[0]).toMatchObject({ local: true, localPreview: false });
  });
});
