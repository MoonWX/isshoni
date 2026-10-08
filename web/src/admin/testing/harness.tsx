// Test support for the admin pages (05 §19.2): a page inside the app's routes and guards, with MSW answering the
// API as 03 §12.4 describes it.
import { configure, screen, within } from '@testing-library/react';
import userEvent, { type UserEvent } from '@testing-library/user-event';
import { http, HttpResponse, type JsonBodyType } from 'msw';
import { vi } from 'vitest';

import type { AppServices } from '../../app/context';
import { createAppRoutes, type AppRouteSources } from '../../app/router';
import type {
  AdminUser,
  AuditEntry,
  Info,
  Invite,
  Me,
  PendingUser,
  Room,
  Settings,
  SettingsResponse,
} from '../../protocol/api.gen';
import { apiPath, infoFixture, meFixture, server, signedIn } from '../../test/msw';
import { createTestServices, renderRoute } from '../../test/render';
// Loaded with the test file, not on the first navigation: the first test of a file doesn't wait for the whole
// folder to be transformed while its findBy… timeout runs.
import * as adminFolder from '../index';

// The admin pages render tables, and finding things by role in a table is slow in jsdom: with every test file of
// the project running at once on a busy machine, a page's first paint can take longer than Testing Library's
// default second, and a test that reads a whole table longer than Vitest's default five. The longer limits cost
// nothing while tests pass. (Per test file: Vitest loads this module once for each.)
configure({ asyncUtilTimeout: 4000 });
vi.setConfig({ testTimeout: 20_000 });

/** The heading of the stand-in for the room. */
export const ROOM_HEADING = 'the room';

function Room() {
  return (
    <main>
      <h1>{ROOM_HEADING}</h1>
    </main>
  );
}

function Login() {
  return (
    <main>
      <h1>login</h1>
    </main>
  );
}

/** The app's routes with the real admin folder, and stand-ins for the room and the login page. */
export function adminSources(): AppRouteSources {
  return {
    lazy: {
      admin: () => Promise.resolve(adminFolder),
      auth: () => Promise.resolve({ LoginPage: Login }),
    },
    eager: { rooms: { RootRedirect: Room, RoomPage: Room } },
  };
}

export interface AdminOptions {
  /** The start path, with its query. */
  path: string;
  /** Who is signed in. Default: an admin (test/msw.ts meFixture). */
  me?: Me;
  /** Overrides of GET /api/v1/info (seeded, as boot does). */
  info?: Partial<Info>;
}

/** Renders the app's routes at an admin path for a signed-in user. */
export function renderAdmin({ path, me = meFixture({ admin: true }), info }: AdminOptions) {
  server.use(signedIn(me));
  const services = createTestServices({ info: infoFixture(info) });
  const result = renderRoute({ path, routes: createAppRoutes(adminSources()), services });
  return { ...result, user: userEvent.setup() };
}

/** The messages of the toasts shown so far (the Toasts region isn't mounted in these tests). */
export function toasts(services: AppServices): string[] {
  return services.ui.getState().toasts.map((toast) => toast.message);
}

/** One request a handler saw. */
export interface Sent {
  /** The path with its query. */
  readonly url: string;
  /** The JSON body; undefined for a GET. */
  readonly body: unknown;
  /** The path parameters (":id"). */
  readonly params: Readonly<Record<string, string | readonly string[] | undefined>>;
}

type Method = 'get' | 'post' | 'patch' | 'delete';

/**
 * Installs a handler that records each request and answers with respond(request). Returns the recorded requests.
 * The path may have parameters: '/api/v1/admin/users/:id'.
 */
export function on(
  method: Method,
  path: `/api/${string}`,
  respond: (sent: Sent) => Response | Promise<Response>,
): Sent[] {
  const seen: Sent[] = [];
  server.use(
    http[method](apiPath(path), async ({ request, params }) => {
      const url = new URL(request.url);
      const body: unknown = method === 'get' ? undefined : await request.json();
      const sent: Sent = { url: url.pathname + url.search, body, params };
      seen.push(sent);
      return respond(sent);
    }),
  );
  return seen;
}

/** A GET that answers with whatever read() returns at that moment, so a test's later changes show after a refetch. */
export function serve(path: `/api/${string}`, read: () => JsonBodyType): Sent[] {
  return on('get', path, () => HttpResponse.json(read()));
}

/** A copy without one field: an invite whose creator was deleted, a user who was never seen. */
export function without<T extends object, K extends keyof T>(value: T, key: K): Omit<T, K> {
  return Object.fromEntries(Object.entries(value).filter(([name]) => name !== key)) as Omit<T, K>;
}

/** A 204 No Content answer. */
export function noContent(): Response {
  return new HttpResponse(null, { status: 204 });
}

/** A failed request: the connection drops before any response. */
export function networkError(): Response {
  return HttpResponse.error();
}

/** The table row whose text matches, for queries inside it. */
export function rowOf(name: string | RegExp): HTMLElement {
  return screen.getByRole('row', { name });
}

/** Opens a users-page row's "⋯" menu and picks an action. */
export async function pickAction(user: UserEvent, username: string, action: string): Promise<void> {
  await user.click(screen.getByRole('button', { name: `Actions for ${username}` }));
  const menu = screen.getByRole('dialog', { name: `Actions for ${username}` });
  await user.click(within(menu).getByRole('button', { name: action }));
}

/** The open modal dialog with this title. */
export function dialog(name: string | RegExp): HTMLElement {
  return screen.getByRole('dialog', { name });
}

export async function typeInto(user: UserEvent, field: HTMLElement, text: string): Promise<void> {
  await user.clear(field);
  if (text !== '') await user.type(field, text);
}

const T0 = '2026-09-30T12:00:00.000Z';

/** One row of GET /api/v1/admin/users: an active member who joined with alex's invite. */
export function adminUser(overrides: Partial<AdminUser> = {}): AdminUser {
  return {
    id: 'b8f2n4r6t0vz',
    username: 'sam',
    role: 'user',
    status: 'active',
    createdVia: 'invite',
    createdAt: T0,
    lastLoginAt: '2026-10-01T18:00:00.000Z',
    lastSeenAt: '2026-10-02T09:30:00.000Z',
    invitedBy: { id: 'a1b2c3d4e5f6', username: 'admin' },
    sessions: 2,
    devices: 0,
    online: false,
    resetPending: false,
    ...overrides,
  };
}

/** The admin of test/msw.ts meFixture({admin: true}), as a users row. */
export function selfUser(overrides: Partial<AdminUser> = {}): AdminUser {
  return {
    id: 'a1b2c3d4e5f6',
    username: 'admin',
    role: 'admin',
    status: 'active',
    createdVia: 'setup',
    createdAt: T0,
    lastLoginAt: '2026-10-01T18:00:00.000Z',
    lastSeenAt: '2026-10-02T09:30:00.000Z',
    sessions: 1,
    devices: 0,
    online: true,
    resetPending: false,
    ...overrides,
  };
}

export function pendingUser(overrides: Partial<PendingUser> = {}): PendingUser {
  return { id: 'p1p2p3p4p5p6', username: 'sam_k', requestedAt: T0, ip: '198.51.100.23', ...overrides };
}

export function invite(overrides: Partial<Invite> = {}): Invite {
  return {
    id: 'h6j8k0m2n4p6',
    note: 'for Sam',
    createdBy: { id: 'a1b2c3d4e5f6', username: 'admin' },
    createdAt: T0,
    expiresAt: '2026-10-07T12:00:00.000Z',
    maxUses: 10,
    uses: 0,
    state: 'active',
    redeemedBy: [],
    ...overrides,
  };
}

export function room(overrides: Partial<Room> = {}): Room {
  return {
    id: 'p4t7w2m9k1qs',
    name: 'Movie night',
    isDefault: false,
    createdAt: T0,
    live: { participants: 0, shares: 0 },
    ...overrides,
  };
}

export const LOUNGE: Room = room({ id: 'lounge', name: 'Lounge', isDefault: true });

/** 03 §9's defaults. */
export const DEFAULT_SETTINGS: Settings = {
  serverName: '',
  registrationMode: 'invite',
  inviteDefaultTtlHours: 168,
  inviteDefaultMaxUses: 10,
  membersCanInvite: false,
  maxParticipantsPerRoom: 0,
  maxSharesPerRoom: 0,
  maxShareBitrateKbps: 0,
  transferAlertGb: 0,
  updateCheck: true,
  minClientVersion: '',
  setupWizardDone: true,
};

/** GET /api/v1/admin/settings: the defaults with these values on top, and these names pinned by config. */
export function settingsResponse(settings: Partial<Settings> = {}, locked: string[] = []): SettingsResponse {
  return { settings: { ...DEFAULT_SETTINGS, ...settings }, defaults: DEFAULT_SETTINGS, locked };
}

/** One audit row: alex made sam an admin (03 §12.4.8's example). */
export function auditEntry(overrides: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id: 4811,
    at: '2026-10-03T19:22:05.114Z',
    action: 'user.role_changed',
    outcome: 'ok',
    actor: { kind: 'user', id: 'a1b2c3d4e5f6', name: 'admin' },
    target: { kind: 'user', id: 'b8f2n4r6t0vz', name: 'sam' },
    ip: '203.0.113.7',
    detail: { from: 'user', to: 'admin' },
    ...overrides,
  };
}
