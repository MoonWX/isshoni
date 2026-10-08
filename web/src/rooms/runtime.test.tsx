// The room runtime and its hooks (05 §7, §11.1): one connection per tab, started by the first room page and kept
// across pages; the route's room becomes the desired room; a server-side redirect navigates; `invalidate` refetches
// REST data; signing out stops the connection with close 1000.
import { act, render, renderHook, screen } from '@testing-library/react';
import { createMemoryRouter, RouterProvider, useParams, type DataRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '../app/App';
import { clearMe } from '../app/me';
import { queryKeys } from '../protocol/queryKeys';
import { ApiError } from '../protocol/rest';
import { BackoffMaxMs } from '../protocol/signal-client';
import { makeError } from '../protocol/testing';
import { connectionBanner } from './connection';
import { roomPath, useConnection, useRoom, useRoomRuntime, useRoomSession } from './hooks';
import { getRoomRuntime } from './runtime';
import { createHarness, FakeShare, pickedSource, refuseConnections, tick, type Harness } from './testing/harness';

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

async function advance(ms = 0): Promise<void> {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

/** A stand-in for the room page: the wiring hook, and what the stores say. */
function RoomProbe() {
  const { roomId } = useParams();
  useRoomSession(roomId);
  const state = useConnection((s) => s.state);
  const joinState = useRoom((s) => s.joinState);
  const name = useRoom((s) => s.room?.name ?? '');
  const people = useRoom((s) => s.state?.participants.length ?? -1);
  return <p data-testid="room">{`${state} ${joinState} ${name} ${String(people)}`}</p>;
}

function renderApp(path: string): DataRouter {
  const router = createMemoryRouter(
    [
      { path: '/r/:roomId', element: <RoomProbe /> },
      { path: '/account', element: <h1>account</h1> },
    ],
    { initialEntries: [path] },
  );
  render(
    <AppProviders services={h.services}>
      <RouterProvider router={router} />
    </AppProviders>,
  );
  return router;
}

const room = () => h.runtime.stores.room.getState();

describe('useRoomSession (05 §7, §11.1)', () => {
  it('makes the route’s room the desired one, starts the connection, and the page reads the stores', async () => {
    renderApp('/r/lounge');
    expect(h.runtime.session.roomId).toBe('lounge');
    expect(h.runtime.signal.state).toBe('connecting');
    expect(screen.getByTestId('room')).toHaveTextContent('connecting joining');
    await advance();
    expect(screen.getByTestId('room')).toHaveTextContent('ready joined Lounge 0');
    expect(h.server.sockets).toHaveLength(1);
    expect(h.hub.joins).toEqual(['lounge']);
  });

  it('keeps the session across pages: no leave on unmount, no second connection or join on the way back', async () => {
    const router = renderApp('/r/lounge');
    await advance();
    await act(() => router.navigate('/account'));
    expect(screen.getByRole('heading', { name: 'account' })).toBeInTheDocument();
    expect(h.runtime.signal.state).toBe('ready');
    expect(room()).toMatchObject({ roomId: 'lounge', joinState: 'joined' });
    expect(h.hub.sent('room.leave')).toEqual([]);

    await act(() => router.navigate('/r/lounge'));
    await advance();
    expect(screen.getByTestId('room')).toHaveTextContent('ready joined Lounge');
    expect(h.server.sockets).toHaveLength(1);
    expect(h.hub.joins).toEqual(['lounge']);
  });

  it('joins the other room when the route changes', async () => {
    const router = renderApp('/r/lounge');
    await advance();
    await act(() => router.navigate('/r/games'));
    await advance();
    expect(screen.getByTestId('room')).toHaveTextContent('ready joined Games');
    expect(h.hub.joins).toEqual(['lounge', 'games']);
  });

  it('follows the session to the default room when the server deleted this one', async () => {
    const router = renderApp('/r/games');
    await advance();
    act(() => {
      h.server.error(makeError('room_closed', 'room', { roomId: 'games' }));
    });
    await advance();
    expect(router.state.location.pathname).toBe('/r/lounge');
    // Replaced, not pushed: Back must not return to the deleted room.
    expect(router.state.historyAction).toBe('REPLACE');
    expect(room()).toMatchObject({ roomId: 'lounge', joinState: 'joined', redirect: null });
    expect(screen.getByTestId('room')).toHaveTextContent('ready joined Lounge');
    expect(h.toasts()).toEqual(['An admin deleted this room.']);
  });

  it('a room that does not exist: the default room, with a toast', async () => {
    h.server.handle('room.join', (data) =>
      data.roomId === 'nope'
        ? { error: makeError('room_not_found', 'request') }
        : { ok: { room: { id: data.roomId, name: 'Lounge' } } },
    );
    const router = renderApp('/r/nope');
    await advance();
    expect(router.state.location.pathname).toBe('/r/lounge');
    expect(h.toasts()).toEqual(["That room doesn't exist anymore."]);
    expect(room()).toMatchObject({ roomId: 'lounge', joinState: 'joined' });
  });

  it('a refused room stays on its page with the error in the store', async () => {
    h.server.handle('room.join', () => ({ error: makeError('room_full', 'request') }));
    const router = renderApp('/r/lounge');
    await advance();
    expect(router.state.location.pathname).toBe('/r/lounge');
    expect(screen.getByTestId('room')).toHaveTextContent('ready failed');
    expect(room().joinError?.code).toBe('room_full');
  });

  it('roomPath encodes the id', () => {
    expect(roomPath('lounge')).toBe('/r/lounge');
    expect(roomPath('a b/c')).toBe('/r/a%20b%2Fc');
  });
});

describe('the runtime', () => {
  it('is one per app: the hooks and getRoomRuntime return the same', () => {
    const { result } = renderHook(() => useRoomRuntime(), {
      wrapper: ({ children }) => <AppProviders services={h.services}>{children}</AppProviders>,
    });
    expect(result.current).toBe(h.runtime);
    expect(getRoomRuntime(h.services)).toBe(h.runtime);
    expect(h.runtime.stores.session).toBe(h.runtime.session);
  });

  it('start() is harmless while the client runs', async () => {
    await h.connect();
    h.runtime.start();
    h.runtime.start();
    await tick(1_000);
    expect(h.server.sockets).toHaveLength(1);
  });

  it('refetches REST data on `invalidate` (05 §6.2)', async () => {
    await h.connect();
    const invalidate = vi.spyOn(h.services.queryClient, 'invalidateQueries');
    h.server.send('invalidate', { topics: ['rooms', 'me'] });
    await tick();
    expect(invalidate.mock.calls.map(([filters]) => filters)).toEqual([
      { queryKey: queryKeys.rooms, exact: false },
      { queryKey: queryKeys.me, exact: true },
    ]);
  });

  it('a REST call answered 503 server_shutdown makes the outage a restart (05 §7.1)', async () => {
    const shuttingDown = new ApiError({ status: 503, code: 'server_shutdown', retryAfterSec: 5 });
    const failingQuery = (key: string) =>
      h.services.queryClient
        .query({ queryKey: ['probe', key], queryFn: () => Promise.reject(shuttingDown), retry: false })
        .catch(() => undefined);
    const banner = () => connectionBanner(h.runtime.stores.connection.getState(), Date.now());
    await h.connect();
    // While the socket is fine it hears server.shutdown itself: a REST answer changes nothing.
    await failingQuery('while ready');
    expect(h.runtime.stores.connection.getState().shutdown).toBeNull();

    // The socket dropped without the announcement; then a REST call learns why.
    refuseConnections(h.server);
    h.server.drop();
    expect(banner()).toBeNull();
    await failingQuery('while down');
    expect(banner()?.kind).toBe('restarting');
    // It holds through the failed attempts in between.
    await tick(5_000);
    expect(banner()?.kind).toBe('restarting');
  });

  it('a failed mutation with server_shutdown counts too; other errors do not', async () => {
    await h.connect();
    refuseConnections(h.server);
    h.server.drop();
    const mutate = (error: Error) =>
      h.services.queryClient
        .getMutationCache()
        .build(h.services.queryClient, { mutationFn: () => Promise.reject(error) })
        .execute(undefined)
        .catch(() => undefined);
    await mutate(new ApiError({ status: 500, code: 'internal' }));
    expect(h.runtime.stores.connection.getState().shutdown).toBeNull();
    await mutate(new ApiError({ status: 503, code: 'server_shutdown' }));
    expect(h.runtime.stores.connection.getState().shutdown).toEqual({ reason: 'restart', reconnectInMs: 0 });
  });

  it('signing out stops the connection with close 1000 and lets go of the room', async () => {
    h.services.queryClient.setQueryData(queryKeys.me, { user: { id: 'k3m9p2qxw7ht' } });
    const joined = h.runtime.session.join('lounge');
    await h.connect();
    await joined;
    const socket = h.server.socket;

    // A logout (here, or in another tab through the BroadcastChannel) makes ['me'] null.
    clearMe(h.services.queryClient);
    expect(h.runtime.signal.state).toBe('stopped');
    // 1000: the server skips the grace period, so friends see the user leave at once (01 §4.2).
    expect(socket.clientClose?.code).toBe(1000);
    await tick();
    expect(room()).toMatchObject({ roomId: null, joinState: 'idle', state: null });
    expect(h.runtime.stores.connection.getState()).toMatchObject({ state: 'stopped', stopReason: null });

    // Logging in again in this tab: the room page starts a new connection.
    h.services.queryClient.setQueryData(queryKeys.me, { user: { id: 'k3m9p2qxw7ht' } });
    const again = h.runtime.session.join('games');
    h.runtime.start();
    await tick();
    await again;
    expect(h.server.welcomes.at(-1)?.resumed).toBe(false);
    expect(room()).toMatchObject({ roomId: 'games', joinState: 'joined' });
  });

  // 05 §15.1: the logout flow stops the share and the client before its request. Its steps are these calls.
  describe('a logout that stops the share and the client before its request (05 §15.1)', () => {
    async function sharingInLounge(): Promise<FakeShare> {
      h.services.queryClient.setQueryData(queryKeys.me, { user: { id: 'k3m9p2qxw7ht' } });
      const joined = h.runtime.session.join('lounge');
      await h.connect();
      await joined;
      const share = new FakeShare('s_local');
      h.sharing.next.push(share);
      await h.runtime.session.startShare(pickedSource, { preset: 'auto', withAudio: true });
      return share;
    }

    it('closes with 1000 before the server revokes the session: no "You were signed out." notice', async () => {
      const share = await sharingInLounge();
      const socket = h.server.socket;
      let stateWhenShareStopped = '';
      const stop = share.stop.bind(share);
      share.stop = () => {
        stateWhenShareStopped = h.runtime.signal.state;
        return stop();
      };

      await h.runtime.session.stopShare();
      h.runtime.signal.stop();
      // The share ended while signaling could still say share.stop; then the close, with nothing from the server.
      expect(stateWhenShareStopped).toBe('ready');
      expect(socket.clientClose?.code).toBe(1000);
      // The request would go out now. The room stays wanted until it succeeded.
      expect(h.runtime.session.roomId).toBe('lounge');

      clearMe(h.services.queryClient);
      await tick();
      expect(room()).toMatchObject({ roomId: null, joinState: 'idle', state: null });
      expect(h.runtime.stores.connection.getState()).toMatchObject({ state: 'stopped', stopReason: null });
      expect(h.toasts()).toEqual([]);
      expect(h.server.sockets).toHaveLength(1);
    });

    it('a logout that failed starts the client again, and the session is back in its room', async () => {
      await sharingInLounge();
      await h.runtime.session.stopShare();
      h.runtime.signal.stop();
      // The request failed: the user is still signed in.
      h.runtime.signal.start();
      await tick();
      expect(h.server.welcomes.at(-1)?.resumed).toBe(false);
      expect(h.hub.joins).toEqual(['lounge', 'lounge']);
      expect(room()).toMatchObject({ roomId: 'lounge', joinState: 'joined' });
      expect(h.toasts()).toEqual([]);
    });
  });

  it('a session the server revoked ends the same way, with the notice', async () => {
    h.services.queryClient.setQueryData(queryKeys.me, { user: { id: 'k3m9p2qxw7ht' } });
    const joined = h.runtime.session.join('lounge');
    await h.connect();
    await joined;
    h.server.error(makeError('session_revoked', 'session'));
    await tick();
    expect(h.services.queryClient.getQueryData(queryKeys.me)).toBeNull();
    expect(room()).toMatchObject({ roomId: null, joinState: 'idle' });
    expect(h.toasts()).toEqual(['You were signed out.']);
    // Stopped for good: no reconnect behind the login page.
    await tick(BackoffMaxMs * 3);
    expect(h.server.sockets).toHaveLength(1);
  });

  it('being signed out before any connection does nothing', () => {
    h.services.queryClient.setQueryData(queryKeys.me, null);
    expect(h.runtime.signal.state).toBe('stopped');
    expect(h.server.sockets).toEqual([]);
  });

  it('dispose() stops the client and the listeners', async () => {
    await h.connect();
    const invalidate = vi.spyOn(h.services.queryClient, 'invalidateQueries');
    const socket = h.server.socket;
    h.runtime.dispose();
    expect(h.runtime.signal.state).toBe('stopped');
    expect(socket.clientClose?.code).toBe(1000);
    h.services.queryClient.setQueryData(queryKeys.me, null);
    expect(invalidate).not.toHaveBeenCalled();
  });
});
