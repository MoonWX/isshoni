// The "/" route (05 §5): to the last room this device joined (localStorage isshoni.lastRoomId) if GET /api/v1/rooms
// still lists it, else to the list's defaultRoomId.
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { useParams } from 'react-router';
import { describe, expect, it } from 'vitest';

import { LAST_ROOM_KEY } from '../app/prefs';
import { createAppRoutes } from '../app/router';
import type { Rooms } from '../protocol/api.gen';
import { apiError, apiPath, roomsFixture, server, signedIn } from '../test/msw';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderRoute } from '../test/render';
import { homeRoomId } from './roomsQuery';
import { RootRedirect } from './RootRedirect';
import { twoRooms } from './testing/page';

function Room() {
  const { roomId } = useParams();
  return <h1>{`room ${roomId ?? ''}`}</h1>;
}

const ROUTES = [
  { path: '/', Component: RootRedirect },
  { path: '/r/:roomId', Component: Room },
];

function serveRooms(rooms: Rooms): void {
  server.use(http.get(apiPath('/api/v1/rooms'), () => HttpResponse.json(rooms)));
}

/** Renders "/" on a device whose localStorage has lastRoomId (none when omitted). */
function renderRoot(lastRoomId?: string) {
  const platform = createTestPlatform();
  if (lastRoomId !== undefined) platform.storage.local.set(LAST_ROOM_KEY, lastRoomId);
  return renderRoute({ path: '/', routes: ROUTES, services: createTestServices({ platform }) });
}

describe('RootRedirect (05 §5)', () => {
  it('reads the last room from the key that 01 §10.5 names', () => {
    expect(LAST_ROOM_KEY).toBe('isshoni.lastRoomId');
  });

  it('goes to the default room of GET /api/v1/rooms on a device that never joined one', async () => {
    serveRooms(roomsFixture({ defaultRoomId: 'h8q2m4x7n1vc' }));
    const { router } = renderRoot();
    expect(await screen.findByRole('heading', { name: 'room h8q2m4x7n1vc' })).toBeInTheDocument();
    // Replaced, not pushed: Back must not come back to "/" and bounce forward again.
    expect(router.state.historyAction).toBe('REPLACE');
  });

  it('goes to isshoni.lastRoomId while that room exists', async () => {
    serveRooms(twoRooms());
    renderRoot('games');
    expect(await screen.findByRole('heading', { name: 'room games' })).toBeInTheDocument();
  });

  it('goes to the default room when the last room is gone', async () => {
    serveRooms(roomsFixture());
    renderRoot('games');
    expect(await screen.findByRole('heading', { name: 'room lounge' })).toBeInTheDocument();
  });

  it('encodes the room id in the path', async () => {
    serveRooms(roomsFixture({ defaultRoomId: 'a b' }));
    const { router } = renderRoot();
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/r/a%20b');
    });
  });

  it('offers to try again while the server can’t be reached, and redirects once it answers', async () => {
    let up = false;
    server.use(
      http.get(apiPath('/api/v1/rooms'), () =>
        up ? HttpResponse.json(roomsFixture()) : apiError(503, { code: 'server_busy' }),
      ),
    );
    renderRoot();
    expect(await screen.findByRole('heading', { name: "Can't reach the server" })).toBeInTheDocument();
    up = true;
    await userEvent.click(screen.getByRole('button', { name: 'Try now' }));
    expect(await screen.findByRole('heading', { name: 'room lounge' })).toBeInTheDocument();
  });

  it('shows the Fatal screen with the code for an answer that a retry can’t fix', async () => {
    server.use(http.get(apiPath('/api/v1/rooms'), () => apiError(403, { code: 'forbidden' })));
    renderRoot();
    expect(await screen.findByRole('heading', { name: 'Something went wrong' })).toBeInTheDocument();
    expect(screen.getByText('Error: forbidden')).toBeInTheDocument();
  });

  it('is what the app’s router renders at "/" for a signed-in user', async () => {
    server.use(signedIn());
    serveRooms(twoRooms());
    const platform = createTestPlatform();
    platform.storage.local.set(LAST_ROOM_KEY, 'games');
    // The real routes with the real RootRedirect; a stand-in room page, which would start a connection.
    const routes = createAppRoutes({ lazy: {}, eager: { rooms: { RootRedirect, RoomPage: Room } } });
    renderRoute({ path: '/', routes, services: createTestServices({ platform }) });
    expect(await screen.findByRole('heading', { name: 'room games' })).toBeInTheDocument();
  });

  it('a signed-out user never gets here: the guard asks for the login first', async () => {
    server.use(signedIn(null));
    const routes = createAppRoutes({ lazy: {}, eager: { rooms: { RootRedirect, RoomPage: Room } } });
    const { router } = renderRoute({ path: '/', routes });
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/login');
    });
  });
});

describe('homeRoomId', () => {
  it('prefers the last room only while the list has it', () => {
    expect(homeRoomId(twoRooms(), 'games')).toBe('games');
    expect(homeRoomId(twoRooms(), 'gone')).toBe('lounge');
    expect(homeRoomId(twoRooms(), null)).toBe('lounge');
  });
});
