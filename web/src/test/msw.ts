// MSW for REST in tests (05 §19.2, README §4 Testing). setup.ts starts `server` for every test file, with
// onUnhandledFrame: 'error', and resets it after each test. A test adds its own handlers with server.use(…).
//
// Handlers match any origin ('*/api/v1/…'): the platform resolves paths against the page's origin, which differs
// between jsdom and node test files.
import { http, HttpResponse } from 'msw';
import { setupServer } from 'msw/node';

import type { Error as ApiErrorBody, Info, Me, Rooms } from '../protocol/api.gen';

/** An /api/v1 path pattern that matches on any origin. */
export function apiPath(path: `/api/${string}`): string {
  return `*${path}`;
}

/** GET /api/v1/info: a set-up server in invite mode, without push. */
export function infoFixture(overrides: Partial<Info> = {}): Info {
  return {
    server: { name: 'Test server', version: '0.0.0-test', publicUrl: 'http://localhost:3000' },
    protocol: { current: 1, min: 1 },
    minClientVersion: '',
    registration: 'invite',
    setupRequired: false,
    features: ['passwordReset'],
    accountRules: { usernameMinLength: 2, usernameMaxLength: 32, passwordMinLength: 8, passwordMaxLength: 128 },
    ...overrides,
  };
}

/** GET /api/v1/me: a member (role user) without invite permission. Pass `admin: true` for an admin. */
export function meFixture({
  admin = false,
  createInvites = admin,
}: { admin?: boolean; createInvites?: boolean } = {}): Me {
  return {
    user: {
      id: admin ? 'a1b2c3d4e5f6' : 'k3m9p2qxw7ht',
      username: admin ? 'admin' : 'alex',
      role: admin ? 'admin' : 'user',
      createdAt: '2026-09-30T12:00:00.000Z',
    },
    session: { id: 's1s2s3s4s5s6', name: 'Chrome on macOS', createdAt: '2026-09-30T12:00:00.000Z', current: true },
    permissions: { admin, createInvites },
    ...(admin ? { badges: { pendingApprovals: 0 } } : {}),
  };
}

/** GET /api/v1/rooms: only the Lounge. */
export function roomsFixture(overrides: Partial<Rooms> = {}): Rooms {
  return {
    defaultRoomId: 'lounge',
    showRoomList: false,
    rooms: [
      {
        id: 'lounge',
        name: 'Lounge',
        isDefault: true,
        createdAt: '2026-09-30T12:00:00.000Z',
        live: { participants: 0, shares: 0 },
      },
    ],
    ...overrides,
  };
}

/** A REST error response in 03 §12.2's envelope; Retry-After is set from retryAfter. */
export function apiError(status: number, error: ApiErrorBody): Response {
  const headers: Record<string, string> = {};
  if (error.retryAfter !== undefined) headers['Retry-After'] = String(error.retryAfter);
  return HttpResponse.json({ error }, { status, headers });
}

/** The handlers every test starts with: /info of a set-up server. /me is not mocked: add it where needed. */
export const defaultHandlers = [http.get(apiPath('/api/v1/info'), () => HttpResponse.json(infoFixture()))];

export const server = setupServer(...defaultHandlers);

/** Handlers for a signed-in user (or, with me: null, a signed-out one: 401 unauthenticated). */
export function signedIn(me: Me | null = meFixture()) {
  return http.get(apiPath('/api/v1/me'), () =>
    me === null ? apiError(401, { code: 'unauthenticated' }) : HttpResponse.json(me),
  );
}
