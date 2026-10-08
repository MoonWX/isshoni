// The room page (05 §11.2) on the real runtime: the SignalClient against the fake signaling server, the session,
// the viewer and the sharer's flow, with REST answered from memory. What the page plugs together is checked from
// the outside: what the user sees, and what reaches the server.
import { act, fireEvent, screen, within } from '@testing-library/react';
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import { createAppRoutes } from '../app/router';
import { LocalErrorCodeRequestTimeout, ProtocolError } from '../protocol/errors';
import { makeError } from '../protocol/testing';
import { shareStore } from '../share/shareStore';
import { shareParams } from '../share/testing/publish';
import { meFixture } from '../test/msw';
import type { RoomHeaderProps } from './RoomHeader';
import {
  createHarness,
  FakeShare,
  participant,
  pickedSource,
  refuseConnections,
  shareInfo,
  type Harness,
  type HarnessOptions,
} from './testing/harness';
import {
  advance,
  PRELOAD_TIMEOUT_MS,
  preloadLazyChunks,
  renderRoomApp,
  stubNativeDialog,
  stubRest,
  twoRooms,
  type RestAnswers,
} from './testing/page';

/** The signed-in user of the fake server's welcome and of meFixture(). */
const ME = 'k3m9p2qxw7ht';
const alex = participant(ME, 'alex');
const bo = participant('u_bo', 'bo');

let h: Harness;

/** Every render of the page's header, in order: the room and the people count it was given. */
const headerRenders = vi.hoisted((): { roomId: string; peopleCount: number | undefined }[] => []);

// The real header, with its renders recorded: one test needs to see a render that may never reach the screen.
vi.mock('./RoomHeader', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./RoomHeader')>();
  return {
    ...actual,
    RoomHeader: (props: RoomHeaderProps) => {
      headerRenders.push({ roomId: props.roomId, peopleCount: props.peopleCount });
      return <actual.RoomHeader {...props} />;
    },
  };
});

// The page's lazy chunks, loaded before the clock is faked.
beforeAll(preloadLazyChunks, PRELOAD_TIMEOUT_MS);

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(Math, 'random').mockReturnValue(0.5); // no backoff jitter
  headerRenders.length = 0;
});

afterEach(() => {
  h.close();
  // The page's share store is one per page (share/shareStore.ts): back to idle for the next test.
  shareStore.setState({ phase: 'idle', withAudio: false, picked: null, params: null, hint: null, error: null });
  vi.useRealTimers();
  vi.restoreAllMocks();
});

/** A harness with REST answers; the page is rendered by open(). */
function setup(rest: RestAnswers = {}, opts: HarnessOptions = {}): void {
  h = createHarness(opts);
  stubRest(h.platform, rest);
}

/** Opens a room's page and waits until the connection is ready and the first room.state is in. */
async function open(path = '/r/lounge') {
  const app = renderRoomApp(h.services, path);
  await advance();
  return app;
}

/** The connection's own id, as the fake server's welcome gave it. */
const ownConnection = (): string => h.runtime.stores.room.getState().connectionId ?? '';

/**
 * The publisher's half of the page's share store (share/shareStore.ts), which share/'s provider drives and the
 * harness's fake share does not: the share was published and is live.
 */
function shareIsLive(shareId: string): void {
  act(() => {
    const store = shareStore.getState();
    store.publishing({
      picked: { kind: 'window', audioScope: 'window', warning: null },
      preset: 'auto',
      withAudio: true,
      params: shareParams(shareId),
    });
    store.advance('live');
  });
}

/** … and it is over. Every test that called shareIsLive() ends with this: the store follows one share at a time. */
function shareIsOver(): void {
  act(() => {
    shareStore.getState().finish();
  });
}

/** The sharer's panel under the stage (05 §13.7). */
const sharePanel = () => within(screen.getByRole('region', { name: 'Your share' }));

function sendState(overrides: Parameters<Harness['hub']['sendState']>[1] = {}): void {
  act(() => {
    h.hub.sendState('lounge', overrides);
  });
}

describe('RoomPage (05 §11.2)', () => {
  it('is what the app’s router renders at /r/:roomId: it joins the route’s room and names it', async () => {
    setup();
    renderRoomApp(h.services, '/r/lounge', createAppRoutes());
    await advance();
    expect(screen.getByRole('heading', { level: 1, name: 'Lounge' })).toBeInTheDocument();
    expect(h.hub.joins).toEqual(['lounge']);
    expect(h.runtime.signal.state).toBe('ready');
  });

  it('shows "Joining…" until the first room.state, then the room', async () => {
    setup();
    // The join is answered, but no snapshot follows yet.
    h.server.handle('room.join', (data) => ({ ok: { room: { id: data.roomId, name: 'Lounge' } } }));
    await open();
    expect(screen.getByRole('status')).toHaveTextContent('Joining…');
    expect(screen.queryByText('Nobody is sharing yet.')).not.toBeInTheDocument();
    // No count before the room said who is here.
    expect(screen.queryByRole('button', { name: /here/ })).not.toBeInTheDocument();

    sendState({ participants: [alex] });
    expect(screen.queryByText('Joining…')).not.toBeInTheDocument();
    expect(screen.getByText('Nobody is sharing yet.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^1 here/ })).toBeInTheDocument();
  });

  it('counts the people ("2 here") and lists them in the people panel', async () => {
    setup();
    await open();
    sendState({ participants: [alex, bo] });
    fireEvent.click(screen.getByRole('button', { name: /^2 here/ }));
    const panel = screen.getByRole('dialog', { name: '2 people here' });
    expect(within(panel).getByText('alex (you)')).toBeInTheDocument();
    expect(within(panel).getByText('bo')).toBeInTheDocument();
  });

  it('marks the admins in the people panel as room.state names them, for a member too', async () => {
    setup(); // GET /me: a member
    await open();
    sendState({ participants: [alex, participant('u_bo', 'bo', { admin: true })] });
    fireEvent.click(screen.getByRole('button', { name: /^2 here/ }));
    const panel = screen.getByRole('dialog', { name: '2 people here' });
    const person = (name: string) => within(panel).getByText(name).closest('li');
    expect(person('bo')).toHaveTextContent('Admin');
    expect(person('alex (you)')).not.toHaveTextContent('Admin');

    // A role change is a new room.state (01 §8.5): the open panel follows it.
    sendState({ participants: [participant(ME, 'alex', { admin: true }), bo] });
    expect(person('alex (you)')).toHaveTextContent('Admin');
    expect(person('bo')).not.toHaveTextContent('Admin');
  });

  it('a click on a person who shares puts their share on the stage, as a pick', async () => {
    setup();
    await open();
    const cy = participant('u_cy', 'cy');
    sendState({
      participants: [alex, bo, cy],
      shares: [
        shareInfo('s_bo', 'u_bo', 'c_bo', { startedAt: '2026-10-12T19:02:30.000Z' }),
        shareInfo('s_cy', 'u_cy', 'c_cy', { startedAt: '2026-10-12T19:05:00.000Z' }),
      ],
    });
    // The newest share is on the stage by itself.
    expect(screen.getByRole('region', { name: "Now watching: cy's window" })).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /^3 here/ }));
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: /bo/ }));
    expect(screen.getByRole('region', { name: "Now watching: bo's window" })).toBeInTheDocument();
    expect(h.runtime.viewer.store.getState()).toMatchObject({ focusedShareId: 's_bo', focusMode: 'manual' });
    // The panel closed itself.
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('the empty state offers to share where the platform can', async () => {
    setup();
    await open();
    expect(screen.getByText('Nobody is sharing yet.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Share your screen' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Share' })).toBeEnabled();
  });

  it('on a phone there is no Share button, and the empty state says why', async () => {
    setup({}, { platform: { sharing: null, role: 'viewer' } });
    await open();
    expect(screen.getByText('Nobody is sharing yet.')).toBeInTheDocument();
    expect(screen.getByText('Sharing from phones needs the app (coming later)')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Share/ })).not.toBeInTheDocument();
  });

  it('gives the viewer every room.state: a friend’s share gets the stage, and the title says "1 live"', async () => {
    setup();
    await open();
    expect(document.title).toBe('Lounge · isshoni');
    sendState({ participants: [alex, bo], shares: [shareInfo('s_bo', 'u_bo', 'c_bo')] });
    expect(screen.getByRole('region', { name: "Now watching: bo's window" })).toBeInTheDocument();
    expect(screen.queryByText('Nobody is sharing yet.')).not.toBeInTheDocument();
    expect(document.title).toBe('● 1 live · Lounge · isshoni');

    // A share that is still starting has no tile and doesn't count (01 §4.4).
    sendState({ participants: [alex, bo], shares: [shareInfo('s_bo2', 'u_bo', 'c_bo', { status: 'starting' })] });
    expect(document.title).toBe('Lounge · isshoni');
    expect(screen.getByText('Nobody is sharing yet.')).toBeInTheDocument();
  });

  it('names the room in the system’s "now playing" for the share that is heard (05 §12.8)', async () => {
    // jsdom has no Media Session API: a stand-in for the two globals the viewer looks for.
    class Metadata {
      readonly title: string;
      readonly artist: string;
      constructor(init: { title?: string; artist?: string } = {}) {
        this.title = init.title ?? '';
        this.artist = init.artist ?? '';
      }
    }
    const mediaSession: { metadata: Metadata | null } = { metadata: null };
    vi.stubGlobal('MediaMetadata', Metadata);
    Object.defineProperty(navigator, 'mediaSession', { value: mediaSession, configurable: true });
    try {
      setup();
      await open();
      expect(mediaSession.metadata).toBeNull();
      sendState({ participants: [alex, bo], shares: [shareInfo('s_bo', 'u_bo', 'c_bo')] });
      expect(mediaSession.metadata).toMatchObject({ title: "bo's window", artist: 'Lounge' });
    } finally {
      Reflect.deleteProperty(navigator, 'mediaSession');
      vi.unstubAllGlobals();
    }
  });

  it('gives the title back when the page is left', async () => {
    setup();
    document.title = 'isshoni';
    const { router } = await open();
    expect(document.title).toBe('Lounge · isshoni');
    await act(() => router.navigate('/account'));
    expect(document.title).toBe('isshoni');
  });

  it('Share publishes through the session, and the share panel’s Stop ends it', async () => {
    setup();
    const share = new FakeShare('s_mine');
    h.sharing.next.push(share);
    h.sharing.pick = () => Promise.resolve(pickedSource);
    await open();
    sendState({ participants: [alex] });
    // No panel while the page shares nothing.
    expect(screen.queryByRole('region', { name: 'Your share' })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Share' }));
    const sheet = screen.getByRole('dialog', { name: 'Share your screen' });
    fireEvent.click(within(sheet).getByRole('button', { name: 'Share' }));
    await advance();
    // session.startShare: platform.sharing.start with the session's signal client and room.
    expect(h.sharing.starts).toHaveLength(1);
    expect(h.sharing.starts[0]?.ctx).toMatchObject({ signal: h.runtime.signal, roomId: 'lounge' });
    expect(h.runtime.session.share).toBe(share);
    expect(document.title).toBe('● Sharing · Lounge · isshoni');
    // The panel shows the share from the start on (05 §13.7), with the session's share to act on.
    expect(sharePanel().getByText('Starting your share…')).toBeInTheDocument();
    expect(sharePanel().getByRole('button', { name: 'Stop sharing' })).toBeEnabled();
    // The header has no Stop of its own.
    expect(screen.getAllByRole('button', { name: 'Stop sharing' })).toHaveLength(1);

    shareIsLive('s_mine');
    fireEvent.click(sharePanel().getByRole('button', { name: 'Stop sharing' }));
    await advance();
    expect(share.calls).toEqual(['stop']);
    expect(h.runtime.session.share).toBeNull();
    shareIsOver();
    expect(screen.queryByRole('region', { name: 'Your share' })).not.toBeInTheDocument();
    expect(document.title).toBe('Lounge · isshoni');
  });

  it('the header offers "Tap to unmute" while the browser refuses to play, and the tap is the viewer’s unlock', async () => {
    setup();
    await open();
    sendState({ participants: [alex, bo], shares: [shareInfo('s_bo', 'u_bo', 'c_bo')] });
    const header = within(screen.getByRole('banner'));
    expect(header.queryByRole('button', { name: 'Tap to unmute' })).not.toBeInTheDocument();

    const unlock = vi.spyOn(h.runtime.viewer, 'unlock').mockImplementation(() => undefined);
    act(() => {
      h.runtime.viewer.store.getState().setAudio('blocked');
    });
    fireEvent.click(header.getByRole('button', { name: 'Tap to unmute' }));
    expect(unlock).toHaveBeenCalledTimes(1);

    act(() => {
      h.runtime.viewer.store.getState().setAudio('playing');
    });
    expect(header.queryByRole('button', { name: 'Tap to unmute' })).not.toBeInTheDocument();
  });

  it('the share panel names who watches this page’s share, from room.state', async () => {
    setup();
    h.sharing.next.push(new FakeShare('s_mine'));
    await open();
    await act(() => h.runtime.session.startShare(pickedSource, { preset: 'auto', withAudio: true }));
    shareIsLive('s_mine');
    sendState({ participants: [alex, bo], shares: [shareInfo('s_mine', ME, ownConnection())] });
    expect(sharePanel().getByText("You're live · Window · Auto · 0 watching")).toBeInTheDocument();

    sendState({
      participants: [alex, bo],
      shares: [
        shareInfo('s_mine', ME, ownConnection(), { watchers: [{ userId: 'u_bo', video: 'high', audio: 'on' }] }),
      ],
    });
    expect(sharePanel().getByText("You're live · Window · Auto · 1 watching")).toBeInTheDocument();
    fireEvent.click(sharePanel().getByRole('button', { name: 'Share settings' }));
    expect(within(sharePanel().getByRole('list')).getByText('bo')).toBeInTheDocument();
    shareIsOver();
  });

  it('tells the user when stopping the share failed', async () => {
    setup();
    const share = new FakeShare('s_mine');
    share.stop = () => Promise.reject(ProtocolError.local(LocalErrorCodeRequestTimeout));
    h.sharing.next.push(share);
    await open();
    await act(() => h.runtime.session.startShare(pickedSource, { preset: 'auto', withAudio: true }));
    shareIsLive('s_mine');
    fireEvent.click(sharePanel().getByRole('button', { name: 'Stop sharing' }));
    await advance();
    expect(h.toasts()).toEqual(["The server didn't answer in time."]);
    shareIsOver();
  });

  it('the share sheet knows about this user’s share on another device, and "Stop it" stops that share', async () => {
    setup();
    await open();
    sendState({
      participants: [alex],
      shares: [shareInfo('s_laptop', ME, 'c_other_device'), shareInfo('s_bo', 'u_bo', 'c_bo')],
    });
    fireEvent.click(screen.getByRole('button', { name: 'Share' }));
    const sheet = screen.getByRole('dialog', { name: 'Share your screen' });
    expect(within(sheet).getByText("You're already sharing from another tab or device")).toBeInTheDocument();
    fireEvent.click(within(sheet).getByRole('button', { name: 'Stop it' }));
    await advance();
    expect(h.hub.sent('share.stop').map((m) => m.data)).toEqual([{ shareId: 's_laptop' }]);
  });

  it('a share of this connection is not "elsewhere"', async () => {
    setup();
    await open();
    sendState({ participants: [alex], shares: [shareInfo('s_mine', ME, ownConnection())] });
    fireEvent.click(screen.getByRole('button', { name: 'Share' }));
    const sheet = screen.getByRole('dialog', { name: 'Share your screen' });
    expect(within(sheet).queryByRole('button', { name: 'Stop it' })).not.toBeInTheDocument();
  });

  it('a refused room shows why, and Try again joins', async () => {
    setup();
    let full = true;
    h.server.handle('room.join', (data) => {
      if (full) return { error: makeError('room_full', 'request') };
      queueMicrotask(() => h.hub.sendState(data.roomId, { participants: [alex] }));
      return { ok: { room: { id: data.roomId, name: 'Lounge' } } };
    });
    await open();
    expect(screen.getByRole('heading', { level: 2, name: "Couldn't join this room" })).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent('This room is full.');
    // Nothing of a room is shown: no stage, no count, and no Share button for a room the user isn't in.
    expect(screen.queryByText('Nobody is sharing yet.')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /here/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Share' })).not.toBeInTheDocument();

    full = false;
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await advance();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByText('Nobody is sharing yet.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Share' })).toBeEnabled();
    expect(h.hub.joins).toEqual(['lounge', 'lounge']);
  });

  it('switching to another room shows that room, and nothing of the one before', async () => {
    setup({ rooms: twoRooms() });
    const { router } = await open();
    sendState({ participants: [alex, bo], shares: [shareInfo('s_bo', 'u_bo', 'c_bo')] });
    expect(h.runtime.viewer.store.getState().shares).toHaveLength(1);

    h.hub.shares = [];
    await act(() => router.navigate('/r/games'));
    await advance();
    expect(screen.getByRole('heading', { level: 1, name: 'Games' })).toBeInTheDocument();
    expect(h.hub.joins).toEqual(['lounge', 'games']);
    expect(h.runtime.viewer.store.getState().shares).toEqual([]);
    expect(screen.getByText('Nobody is sharing yet.')).toBeInTheDocument();
    expect(document.title).toBe('Games · isshoni');
  });

  it('never renders the people of the room before under the next room’s id, not even once', async () => {
    setup({ rooms: twoRooms() });
    const { router } = await open();
    sendState({ participants: [alex, bo] });
    expect(headerRenders.at(-1)).toEqual({ roomId: 'lounge', peopleCount: 2 });

    h.hub.shares = [];
    await act(() => router.navigate('/r/games'));
    await advance();
    // The first render for /r/games comes before the session took that room (an effect does that): the store
    // still holds the Lounge's snapshot then, and the page must not show it.
    const forGames = headerRenders.filter((r) => r.roomId === 'games').map((r) => r.peopleCount);
    expect(forGames[0]).toBeUndefined();
    expect(forGames).not.toContain(2);
    expect(forGames.at(-1)).toBe(0);
  });

  it('shows the room’s new name after a rename (the `rooms` topic refetches the list)', async () => {
    let name = 'Lounge';
    setup({ rooms: () => twoRooms({ rooms: [{ ...lounge(), name }] }) });
    await open();
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Lounge');
    name = 'Living room';
    act(() => {
      h.server.send('invalidate', { topics: ['rooms'] });
    });
    await advance();
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Living room');
    expect(document.title).toBe('Living room · isshoni');
  });

  it('has the connection banner, whose "Test my connection" opens the connection test', async () => {
    setup();
    await open();
    refuseConnections(h.server);
    act(() => {
      h.server.drop();
    });
    await advance(30_000);
    const banner = screen.getByTestId('connection-banner');
    expect(banner).toHaveTextContent("Can't reach the server. Retrying…");
    fireEvent.click(within(banner).getByRole('button', { name: 'Test my connection' }));
    await advance();
    const dialog = screen.getByRole('dialog', { name: 'Test my connection' });
    // The panel waits for its button: opening the dialog starts no test.
    expect(within(dialog).getByRole('button', { name: 'Test my connection' })).toBeEnabled();
    expect(within(dialog).getByText(/whether video can travel/)).toBeInTheDocument();

    fireEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('the account menu’s "Test my connection" opens the connection test (05 §14.2)', async () => {
    setup();
    await open();
    // No outage and no banner: the menu is the way to the test.
    expect(screen.getByTestId('connection-banner')).not.toHaveAttribute('data-kind');
    fireEvent.click(screen.getByRole('button', { name: 'Account menu, alex' }));
    const menu = screen.getByRole('dialog', { name: 'Account menu' });
    fireEvent.click(within(menu).getByRole('button', { name: 'Test my connection' }));
    await advance();
    expect(screen.queryByRole('dialog', { name: 'Account menu' })).not.toBeInTheDocument();
    const dialog = screen.getByRole('dialog', { name: 'Test my connection' });
    expect(within(dialog).getByRole('button', { name: 'Test my connection' })).toBeEnabled();
  });

  it('has the account menu of the signed-in user', async () => {
    setup({ me: meFixture({ admin: true }) });
    await open();
    fireEvent.click(screen.getByRole('button', { name: 'Account menu, admin' }));
    const menu = screen.getByRole('dialog', { name: 'Account menu' });
    expect(within(menu).getByText('Signed in as admin')).toBeInTheDocument();
    expect(within(menu).getByRole('link', { name: 'Admin' })).toHaveAttribute('href', '/admin');
  });
});

describe('RoomPage: its dialogs and the keyboard focus (05 §16.6)', () => {
  // A native <dialog> gives the focus back to the control that opened it only when it is closed while it is in the
  // page. jsdom has no native dialog: the stub records what each close() found.
  let dialogs: ReturnType<typeof stubNativeDialog>;

  beforeEach(() => {
    dialogs = stubNativeDialog();
  });

  afterEach(() => {
    dialogs.restore();
  });

  it('closes the people panel in place', async () => {
    setup();
    await open();
    sendState({ participants: [alex, bo] });
    fireEvent.click(screen.getByRole('button', { name: /^2 here/ }));
    const panel = screen.getByRole('dialog', { name: '2 people here' });
    fireEvent.click(within(panel).getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(dialogs.closes).toEqual([{ title: '2 people here', inPage: true }]);
  });

  it('closes the people panel in place when a click on a person closes it', async () => {
    setup();
    await open();
    sendState({ participants: [alex, bo], shares: [shareInfo('s_bo', 'u_bo', 'c_bo')] });
    fireEvent.click(screen.getByRole('button', { name: /^2 here/ }));
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: /bo/ }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(dialogs.closes).toEqual([{ title: '2 people here', inPage: true }]);
  });

  it('closes the people panel in place when the room’s snapshot goes', async () => {
    setup({ rooms: twoRooms() });
    const { router } = await open();
    sendState({ participants: [alex, bo] });
    fireEvent.click(screen.getByRole('button', { name: /^2 here/ }));
    expect(screen.getByRole('dialog', { name: '2 people here' })).toBeInTheDocument();

    // Another room: the list of the one before is gone, and the panel with it. It doesn't come back by itself.
    await act(() => router.navigate('/r/games'));
    await advance();
    expect(screen.getByRole('heading', { level: 1, name: 'Games' })).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(dialogs.closes.map((c) => c.inPage)).toEqual([true]);
  });

  it('closes the connection test in place', async () => {
    setup();
    await open();
    fireEvent.click(screen.getByRole('button', { name: 'Account menu, alex' }));
    fireEvent.click(screen.getByRole('button', { name: 'Test my connection' }));
    await advance();
    const dialog = screen.getByRole('dialog', { name: 'Test my connection' });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(dialogs.closes).toEqual([{ title: 'Test my connection', inPage: true }]);
  });

  it('lists nobody in the closed people panel', async () => {
    setup();
    await open();
    sendState({ participants: [alex, bo] });
    // The drawer is in the page all the time, closed: its rows must not be found there by a search for a name.
    expect(screen.queryByText('alex (you)')).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /^2 here/ }));
    expect(screen.getByText('alex (you)')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(screen.queryByText('alex (you)')).not.toBeInTheDocument();
  });
});

/** The Lounge of twoRooms(). */
function lounge() {
  const room = twoRooms().rooms[0];
  if (room === undefined) throw new Error('twoRooms() has no rooms');
  return room;
}
