// Test support for the room's pages: the REST answers they read, served from memory, and a small app around them
// (the real RoomPage, RootRedirect and InRoomBar, with stand-ins for the pages of other folders). Tests that also
// need signaling build on the harness (harness.ts) and its fake timers, where a request through MSW would not come
// back: stubRest answers inside the platform, without a network. Only tests import this folder.
import { act, render, type RenderResult } from '@testing-library/react';
import { createMemoryRouter, Outlet, RouterProvider, type DataRouter, type RouteObject } from 'react-router';
import { vi, type MockInstance } from 'vitest';

import { AppProviders } from '../../app/App';
import type { AppServices } from '../../app/context';
import type { Platform } from '../../platform/types';
import type { Me, Rooms } from '../../protocol/api.gen';
import { meFixture, roomsFixture } from '../../test/msw';
import { InRoomBar } from '../InRoomBar';
import { loadRoomMedia } from '../loadMedia';
import { RoomPage } from '../RoomPage';
import { RootRedirect } from '../RootRedirect';

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

export interface RestAnswers {
  /** GET /api/v1/me; null answers 401 unauthenticated. Default: meFixture(). */
  me?: Me | null;
  /** GET /api/v1/rooms, read again on every request. Default: roomsFixture() (only the Lounge, no room list). */
  rooms?: Rooms | (() => Rooms);
}

/**
 * Answers the platform's REST calls from memory: /me and /rooms as given, 404 not_found for anything else. Returns
 * the spy on platform.apiFetch, whose calls are the requests made.
 */
export function stubRest(platform: Platform, answers: RestAnswers = {}): MockInstance<Platform['apiFetch']> {
  const me = answers.me === undefined ? meFixture() : answers.me;
  const rooms = answers.rooms ?? roomsFixture();
  return vi.spyOn(platform, 'apiFetch').mockImplementation((path) => {
    if (path === '/api/v1/me') {
      return Promise.resolve(me === null ? json({ error: { code: 'unauthenticated' } }, 401) : json(me));
    }
    if (path === '/api/v1/rooms') return Promise.resolve(json(typeof rooms === 'function' ? rooms() : rooms));
    return Promise.resolve(json({ error: { code: 'not_found' } }, 404));
  });
}

/** Two rooms, so the room list shows (03 §8): the Lounge and Games. */
export function twoRooms(overrides: Partial<Rooms> = {}): Rooms {
  return roomsFixture({
    showRoomList: true,
    rooms: [
      {
        id: 'lounge',
        name: 'Lounge',
        isDefault: true,
        createdAt: '2026-09-30T12:00:00.000Z',
        live: { participants: 3, shares: 1 },
      },
      {
        id: 'games',
        name: 'Games',
        isDefault: false,
        createdAt: '2026-10-01T12:00:00.000Z',
        live: { participants: 1, shares: 0 },
      },
    ],
    ...overrides,
  });
}

/** The app's layout as far as the room goes: InRoomBar above every page. */
function Layout() {
  return (
    <>
      <InRoomBar />
      <Outlet />
    </>
  );
}

/** The room's pages, and headings where the pages of other folders would be. */
export const ROOM_ROUTES: RouteObject[] = [
  {
    Component: Layout,
    children: [
      { path: '/', Component: RootRedirect },
      { path: '/r/:roomId', Component: RoomPage },
      { path: '/account', element: <h1>Account page</h1> },
      { path: '/account/notifications', element: <h1>Notifications page</h1> },
      { path: '/admin', element: <h1>Admin page</h1> },
      { path: '/admin/rooms', element: <h1>Admin rooms page</h1> },
      { path: '/admin/invites', element: <h1>Invites page</h1> },
      { path: '/about', element: <h1>About page</h1> },
      { path: '/login', element: <h1>Login page</h1> },
    ],
  },
];

/** Renders ROOM_ROUTES (or a test's own routes) at path, inside the app's providers. */
export function renderRoomApp(
  services: AppServices,
  path: string,
  routes: RouteObject[] = ROOM_ROUTES,
): RenderResult & { router: DataRouter } {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  const result = render(
    <AppProviders services={services}>
      <RouterProvider router={router} />
    </AppProviders>,
  );
  return { ...result, router };
}

/** Advances the fake clock inside act(), so timers, store updates and resolved requests render. */
export async function advance(ms = 0): Promise<void> {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

/**
 * Loads the room page's lazy chunks: the media chunk (the stage, the Share button, the sub PC) and the connection
 * test's panel. Call it in beforeAll, with PRELOAD_TIMEOUT_MS: a first load reads and transforms files, which
 * takes real time that no fake timer advances, and on a busy machine more than a hook's default timeout. Once
 * loaded, the page's own import() calls resolve at once.
 */
export async function preloadLazyChunks(): Promise<void> {
  await loadRoomMedia();
  await import('../../conntest/ConnTestPanel');
}

export const PRELOAD_TIMEOUT_MS = 60_000;
