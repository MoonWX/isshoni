// InRoomBar (05 §11.1): away from the room page the session goes on, and the bar says so: "In Lounge · sharing ·
// [Back] [Leave]". On a room's page, without a room, and in an app that never opened one, there is no bar.
import { act, fireEvent, screen, within } from '@testing-library/react';
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import { createAppRoutes } from '../app/router';
import { makeError } from '../protocol/testing';
import { shareStore } from '../share/shareStore';
import { createTestServices } from '../test/render';
import { isRoomPath } from './hooks';
import { peekRoomRuntime } from './runtime';
import { createHarness, FakeShare, pickedSource, refuseConnections, type Harness } from './testing/harness';
import {
  advance,
  PRELOAD_TIMEOUT_MS,
  preloadLazyChunks,
  renderRoomApp,
  stubRest,
  twoRooms,
  type RestAnswers,
} from './testing/page';

let h: Harness | undefined;

// The room page's lazy chunks, loaded before the clock is faked; and the folder of /about, for the app's routes.
beforeAll(async () => {
  await preloadLazyChunks();
  await import('../auth');
}, PRELOAD_TIMEOUT_MS);

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(Math, 'random').mockReturnValue(0.5); // no backoff jitter
});

afterEach(() => {
  h?.close();
  h = undefined;
  shareStore.setState({ phase: 'idle', withAudio: false, picked: null, params: null, hint: null, error: null });
  vi.useRealTimers();
  vi.restoreAllMocks();
});

const bar = (): HTMLElement | null => screen.queryByRole('region', { name: 'Your room' });

/** The app in the Lounge, on the room page. */
async function inLounge(rest: RestAnswers = {}) {
  h = createHarness();
  stubRest(h.platform, rest);
  const app = renderRoomApp(h.services, '/r/lounge');
  await advance();
  expect(h.runtime.stores.room.getState()).toMatchObject({ roomId: 'lounge', joinState: 'joined' });
  return { ...app, h };
}

describe('InRoomBar (05 §11.1)', () => {
  it('is in the app’s own layout: above another page of the real routes, and not on the room’s page', async () => {
    h = createHarness();
    stubRest(h.platform);
    const { router } = renderRoomApp(h.services, '/r/lounge', createAppRoutes());
    await advance();
    expect(screen.getByRole('heading', { level: 1, name: 'Lounge' })).toBeInTheDocument();
    expect(bar()).not.toBeInTheDocument();

    // /about is a page for everyone.
    await act(() => router.navigate('/about'));
    await advance();
    const region = bar();
    expect(region).toHaveTextContent('In Lounge');
    expect(within(region as HTMLElement).getByRole('link', { name: 'Back' })).toHaveAttribute('href', '/r/lounge');
    // The bar is above the page, not inside it.
    const page = screen.getByRole('heading', { level: 1 });
    expect((region as HTMLElement).compareDocumentPosition(page) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(h.runtime.signal.state).toBe('ready');

    fireEvent.click(within(region as HTMLElement).getByRole('button', { name: 'Leave' }));
    await advance();
    expect(bar()).not.toBeInTheDocument();
    expect(h.hub.sent('room.leave')).toHaveLength(1);
  });

  it('is not there on the room’s own page', async () => {
    await inLounge();
    expect(screen.getByRole('heading', { level: 1, name: 'Lounge' })).toBeInTheDocument();
    expect(bar()).not.toBeInTheDocument();
  });

  it('shows the room on other pages, with Back and Leave', async () => {
    const { router, h } = await inLounge();
    await act(() => router.navigate('/account'));
    expect(screen.getByRole('heading', { name: 'Account page' })).toBeInTheDocument();
    const region = bar();
    expect(region).toHaveTextContent('In Lounge');
    // Not sharing: the bar doesn't say so.
    expect(region).not.toHaveTextContent('sharing');
    expect(within(region as HTMLElement).getByRole('link', { name: 'Back' })).toHaveAttribute('href', '/r/lounge');
    expect(within(region as HTMLElement).getByRole('button', { name: 'Leave' })).toBeEnabled();
    // The session went on: no leave, no second connection.
    expect(h.hub.sent('room.leave')).toEqual([]);
    expect(h.runtime.signal.state).toBe('ready');
  });

  it('says "sharing" while this tab still publishes', async () => {
    const { router, h } = await inLounge();
    await act(() => h.runtime.session.startShare(pickedSource, { preset: 'auto', withAudio: true }));
    await act(() => router.navigate('/account'));
    expect(bar()).toHaveTextContent('In Lounge');
    expect(bar()).toHaveTextContent('sharing');

    await act(() => h.runtime.session.stopShare());
    expect(bar()).not.toHaveTextContent('sharing');
  });

  it('Back returns to the room’s page without joining again', async () => {
    const { router, h } = await inLounge();
    await act(() => router.navigate('/account'));
    fireEvent.click(screen.getByRole('link', { name: 'Back' }));
    await advance();
    expect(router.state.location.pathname).toBe('/r/lounge');
    expect(bar()).not.toBeInTheDocument();
    expect(h.hub.joins).toEqual(['lounge']);
    expect(h.server.sockets).toHaveLength(1);
  });

  it('Leave stops the share, leaves the room on the server, and the bar goes', async () => {
    const { router, h } = await inLounge();
    const share = new FakeShare('s_mine');
    h.sharing.next.push(share);
    await act(() => h.runtime.session.startShare(pickedSource, { preset: 'auto', withAudio: true }));
    await act(() => router.navigate('/account'));

    fireEvent.click(screen.getByRole('button', { name: 'Leave' }));
    await advance();
    expect(share.calls).toEqual(['stop']);
    expect(h.hub.sent('room.leave')).toHaveLength(1);
    expect(h.runtime.session.roomId).toBeNull();
    expect(bar()).not.toBeInTheDocument();
    // The page under it stays.
    expect(screen.getByRole('heading', { name: 'Account page' })).toBeInTheDocument();
  });

  it('carries the connection banner, so an outage shows on other pages too', async () => {
    const { router, h } = await inLounge();
    await act(() => router.navigate('/account'));
    refuseConnections(h.server);
    act(() => {
      h.server.drop();
    });
    await advance(2_000);
    expect(within(bar() as HTMLElement).getByTestId('connection-banner')).toHaveTextContent('Reconnecting…');
  });

  it('is not there for a room that refused the user: they are in no room', async () => {
    h = createHarness();
    stubRest(h.platform, { rooms: twoRooms() });
    h.server.handle('room.join', () => ({ error: makeError('room_full', 'request') }));
    const { router } = renderRoomApp(h.services, '/r/games');
    await advance();
    expect(screen.getByRole('alert')).toHaveTextContent('This room is full.');
    // The refused room is still the session's desired one, for the page's "Try again".
    expect(h.runtime.stores.room.getState()).toMatchObject({ roomId: 'games', joinState: 'failed' });

    await act(() => router.navigate('/account'));
    await advance();
    expect(screen.getByRole('heading', { name: 'Account page' })).toBeInTheDocument();
    // Nothing to go back to, and nothing to leave.
    expect(bar()).not.toBeInTheDocument();
  });

  it('names the room from the room list while the join is still under way', async () => {
    // No welcome yet: the join waits for it, and the session has no name for its room.
    h = createHarness({ server: { autoWelcome: false } });
    stubRest(h.platform, { rooms: twoRooms() });
    const { router } = renderRoomApp(h.services, '/r/games');
    await advance();
    expect(h.runtime.stores.room.getState()).toMatchObject({ roomId: 'games', joinState: 'joining', room: null });

    await act(() => router.navigate('/account'));
    await advance();
    expect(bar()).toHaveTextContent('In Games');
    expect(bar()).not.toHaveTextContent('In Room');
  });

  it('shows the room’s name of today after a rename, as the room’s page does', async () => {
    let name = 'Lounge';
    const lounge = twoRooms().rooms[0];
    if (lounge === undefined) throw new Error('twoRooms() has no rooms');
    const { router, h } = await inLounge({ rooms: () => twoRooms({ rooms: [{ ...lounge, name }] }) });
    await act(() => router.navigate('/account'));
    expect(bar()).toHaveTextContent('In Lounge');

    name = 'Living room';
    act(() => {
      h.server.send('invalidate', { topics: ['rooms'] });
    });
    await advance();
    expect(bar()).toHaveTextContent('In Living room');
  });

  it('is not there while the session is in no room', async () => {
    h = createHarness();
    stubRest(h.platform);
    // A runtime exists (the harness made it), but nothing joined a room.
    renderRoomApp(h.services, '/account');
    await advance();
    expect(bar()).not.toBeInTheDocument();
  });

  it('never makes a runtime: an app that didn’t open a room starts no connection for it', async () => {
    const services = createTestServices();
    renderRoomApp(services, '/account');
    await advance();
    expect(screen.getByRole('heading', { name: 'Account page' })).toBeInTheDocument();
    expect(bar()).not.toBeInTheDocument();
    expect(peekRoomRuntime(services)).toBeUndefined();
  });
});

describe('isRoomPath', () => {
  it('matches a room’s page and nothing else', () => {
    expect(isRoomPath('/r/lounge')).toBe(true);
    expect(isRoomPath('/r/k3m9p2qxw7ht/')).toBe(true);
    expect(isRoomPath('/r/a%20b')).toBe(true);
    expect(isRoomPath('/')).toBe(false);
    expect(isRoomPath('/r')).toBe(false);
    expect(isRoomPath('/r/')).toBe(false);
    expect(isRoomPath('/r/lounge/extra')).toBe(false);
    expect(isRoomPath('/account')).toBe(false);
    expect(isRoomPath('/admin/rooms')).toBe(false);
  });
});
