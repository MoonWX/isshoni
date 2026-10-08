// Test support for the account pages (05 §19.2): a page inside the app's routes, with MSW answering the API.
// It builds on the auth pages' harness: the same stand-in for the room, the real auth folder (a sign-out ends on
// the real login page), and here the real account folder.
import { http, HttpResponse } from 'msw';

// The two page folders these tests route through, loaded with the test file rather than by the first test's
// navigation: the router's lazy import then finds them ready, and on a slow machine the first test's findBy…
// doesn't time out waiting for it.
import '../../auth/index';
import '../index';

import { createAppRoutes, type AppRouteSources } from '../../app/router';
import { mockSession, pageSources, type SessionState } from '../../auth/testing/harness';
import type { Platform } from '../../platform/types';
import type { DeviceInfo, Info, Me, SessionInfo } from '../../protocol/api.gen';
import { apiPath, infoFixture, meFixture, server } from '../../test/msw';
import { createTestPlatform } from '../../test/platform';
import { createTestServices, renderRoute } from '../../test/render';

/** The heading of the real login page for infoFixture()'s server: where a sign-out ends. */
export const LOGIN_HEADING = 'Log in to Test server';

/** The app's routes with the real account and auth folders and a stand-in for the room. */
export function accountSources(): AppRouteSources {
  const base = pageSources();
  return { ...base, lazy: { ...base.lazy, account: () => import('../index') } };
}

export interface AccountPageOptions {
  /** Who is signed in; default meFixture() (alex, a member). */
  me?: Me;
  /** Overrides of GET /api/v1/info (seeded, as boot does). */
  info?: Partial<Info>;
  platform?: Platform;
}

/**
 * Renders an account route for a signed-in user. `session` is the server's side: a handler that ends the session
 * sets `session.me = null`, and GET /api/v1/me answers 401 from then on.
 */
export function renderAccount(path: string, { me = meFixture(), info, platform }: AccountPageOptions = {}) {
  const session: SessionState = mockSession(me);
  const services = createTestServices({ platform: platform ?? createTestPlatform(), info: infoFixture(info) });
  return { ...renderRoute({ path, routes: createAppRoutes(accountSources()), services }), session };
}

/** Installs a GET handler that answers with respond(). Returns how often it was asked. */
export function onGet(path: `/api/${string}`, respond: () => Response | Promise<Response>): { calls: number } {
  const seen = { calls: 0 };
  server.use(
    http.get(apiPath(path), () => {
      seen.calls++;
      return respond();
    }),
  );
  return seen;
}

/**
 * Installs a DELETE handler for a path with one `:id` segment ("/api/v1/me/sessions/:id") that answers with
 * respond(id). Returns the ids that were asked for.
 */
export function onDelete(path: `/api/${string}`, respond: (id: string) => Response | Promise<Response>): string[] {
  const ids: string[] = [];
  server.use(
    http.delete(apiPath(path), ({ params }) => {
      const id = String(params['id']);
      ids.push(id);
      return respond(id);
    }),
  );
  return ids;
}

/** One row of GET /api/v1/me/sessions; `seenAgoMs` sets lastSeenAt relative to now. */
export function sessionFixture(
  overrides: Partial<SessionInfo> & { seenAgoMs?: number } = {},
  now: number = Date.now(),
): SessionInfo {
  const { seenAgoMs = 0, ...rest } = overrides;
  return {
    id: 's1s2s3s4s5s6',
    name: 'Chrome on macOS',
    createdAt: '2026-09-30T12:00:00.000Z',
    lastSeenAt: new Date(now - seenAgoMs).toISOString(),
    lastIp: '203.0.113.7',
    current: false,
    ...rest,
  };
}

/** One row of GET /api/v1/me/devices (M2's shape, 03 §12.4.3). */
export function deviceFixture(overrides: Partial<DeviceInfo> = {}): DeviceInfo {
  return {
    id: 'd1d2d3d4d5d6',
    name: 'Alex-PC',
    clientKind: 'desktop',
    os: 'windows',
    appVersion: '0.2.0',
    createdAt: '2026-09-30T12:00:00.000Z',
    lastSeenAt: new Date().toISOString(),
    lastIp: '203.0.113.7',
    ...overrides,
  };
}

/** A server-side list of sessions that GET, DELETE and revoke-others act on, as the real endpoints do. */
export interface SessionList {
  sessions: SessionInfo[];
  /** How often GET /api/v1/me/sessions was asked. */
  readonly gets: { calls: number };
  /** The ids DELETE /api/v1/me/sessions/{id} was asked for. */
  readonly deleted: string[];
}

/** Installs GET /api/v1/me/sessions and DELETE /api/v1/me/sessions/{id} over one list. */
export function mockSessions(initial: SessionInfo[]): SessionList {
  const list: SessionList = {
    sessions: initial,
    gets: onGet('/api/v1/me/sessions', () => HttpResponse.json({ sessions: list.sessions })),
    deleted: onDelete('/api/v1/me/sessions/:id', (id) => {
      list.sessions = list.sessions.filter((s) => s.id !== id);
      return new HttpResponse(null, { status: 204 });
    }),
  };
  return list;
}

/** Installs GET /api/v1/me/devices. M1's answer is the empty list. */
export function mockDevices(devices: DeviceInfo[] = []): { calls: number } {
  return onGet('/api/v1/me/devices', () => HttpResponse.json({ devices }));
}
