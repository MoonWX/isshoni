// Test support for the auth and setup pages (05 §19.2): a page inside the app's routes, with MSW answering the API.
import { screen } from '@testing-library/react';
import type { UserEvent } from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';

import { fragmentTokenKey, type FragmentTokenKind } from '../../app/boot';
import { createAppRoutes, type AppRouteSources } from '../../app/router';
import type { Platform } from '../../platform/types';
import type { Error as ApiErrorBody, Info, Me } from '../../protocol/api.gen';
import { apiError, apiPath, infoFixture, server } from '../../test/msw';
import { createTestPlatform } from '../../test/platform';
import { createTestServices, renderRoute } from '../../test/render';

/** The heading of the stand-in for the room, where "/" ends up for a signed-in user. */
export const ROOM_HEADING = 'the room';

function Room() {
  return (
    <main>
      <h1>{ROOM_HEADING}</h1>
    </main>
  );
}

/** The app's routes with the real auth and setup folders, a stand-in for the room, and one guarded page. */
export function pageSources(): AppRouteSources {
  return {
    lazy: {
      auth: () => import('../index'),
      setup: () => import('../../setup/index'),
      account: () => Promise.resolve({ AccountPage: () => <h1>account</h1> }),
    },
    eager: { rooms: { RootRedirect: Room, RoomPage: Room } },
  };
}

export interface PageOptions {
  /** The start path, with its query. */
  path: string;
  /** The token boot would have stashed from the fragment (05 §4 step 0). */
  token?: readonly [FragmentTokenKind, string];
  /** Overrides of GET /api/v1/info (seeded, as boot does). */
  info?: Partial<Info>;
  platform?: Platform;
}

/** Renders the app's routes at a path, with the token (if any) already in the tab's session storage. */
export function renderPage({ path, token, info, platform = createTestPlatform() }: PageOptions) {
  if (token) platform.storage.session.set(fragmentTokenKey(token[0]), token[1]);
  const services = createTestServices({ platform, info: infoFixture(info) });
  return { ...renderRoute({ path, routes: createAppRoutes(pageSources()), services }), platform };
}

/** The server's side of the session: GET /api/v1/me answers 401 unauthenticated until `me` is set. */
export interface SessionState {
  me: Me | null;
  /** How often GET /api/v1/me was asked. */
  meCalls: number;
}

/** Installs GET /api/v1/me for a session that tests switch on and off (a login handler sets state.me). */
export function mockSession(initial: Me | null = null): SessionState {
  const state: SessionState = { me: initial, meCalls: 0 };
  server.use(
    http.get(apiPath('/api/v1/me'), () => {
      state.meCalls++;
      return state.me ? HttpResponse.json(state.me) : apiError(401, { code: 'unauthenticated' });
    }),
  );
  return state;
}

/**
 * Installs a POST handler that records each JSON body and answers with respond(body). Returns the recorded bodies.
 */
export function onPost(path: `/api/${string}`, respond: (body: unknown) => Response | Promise<Response>): unknown[] {
  const bodies: unknown[] = [];
  server.use(
    http.post(apiPath(path), async ({ request }) => {
      const body: unknown = await request.json();
      bodies.push(body);
      return respond(body);
    }),
  );
  return bodies;
}

/** A 204 No Content answer. */
export function noContent(): Response {
  return new HttpResponse(null, { status: 204 });
}

/** A failed request: the connection drops before any response. */
export function networkError(): Response {
  return HttpResponse.error();
}

export async function typeInto(user: UserEvent, label: string, text: string): Promise<void> {
  const input = screen.getByLabelText(label);
  await user.clear(input);
  if (text !== '') await user.type(input, text);
}

/** Fills the username and password of a new-account form. */
export async function fillAccount(
  user: UserEvent,
  username = 'alex',
  password = 'correct horse battery',
): Promise<void> {
  await typeInto(user, 'Username', username);
  await typeInto(user, 'Password', password);
}

/** One row of an error table: the response, and what the form shows for it. */
export type ErrorRow = readonly [name: string, status: number, error: ApiErrorBody, shown: string | RegExp];

/**
 * The errors every public auth endpoint can answer (03 §12.3 "Common errors", §7.3), with the text shown above the
 * form.
 */
export const COMMON_ERRORS: readonly ErrorRow[] = [
  ['bad_request', 400, { code: 'bad_request' }, "The server couldn't accept that request."],
  ['payload_too_large', 413, { code: 'payload_too_large' }, "That's too much data for one request."],
  ['unsupported_media_type', 415, { code: 'unsupported_media_type' }, /couldn't read that request/],
  ['csrf_failed', 403, { code: 'csrf_failed' }, /seemed to come from another site/],
  ['rate_limited without a wait', 429, { code: 'rate_limited' }, 'Too many attempts. Try again in a moment.'],
  ['server_busy without a wait', 503, { code: 'server_busy' }, 'The server is busy. Try again in a moment.'],
  [
    'internal, with its reference',
    500,
    { code: 'internal', requestId: 'req-7f3a' },
    'Something went wrong on the server. Reference: req-7f3a',
  ],
  ['server_shutdown', 503, { code: 'server_shutdown' }, 'The server is restarting.'],
  ['a code this build does not know', 418, { code: 'brand_new_code' }, 'Something went wrong (brand_new_code).'],
];

/**
 * 2xx answers to a link check that aren't the server's: something between the browser and the server answered (a
 * captive portal's page, a proxy), or the body isn't the DTO.
 */
export const NOT_THE_ANSWER: readonly (readonly [name: string, respond: () => Response])[] = [
  ['a web page', () => new HttpResponse('<html></html>', { status: 200, headers: { 'Content-Type': 'text/html' } })],
  ['JSON null', () => HttpResponse.json(null)],
  ['JSON of something else', () => HttpResponse.json({ ok: true })],
];

/** One row of a field table: the field code the server (or the form's own check) gives, and the text under it. */
export type FieldRow = readonly [code: string, shown: string];

/** 03 §7.1's username codes. */
export const USERNAME_CODES: readonly FieldRow[] = [
  ['required', 'Enter a username.'],
  ['too_short', "That's too short for a username."],
  ['too_long', "That's too long for a username."],
  ['invalid', 'Use letters and numbers, with single dots, dashes or underscores in between.'],
  ['reserved', 'That username is reserved. Pick another.'],
  // A field code without a text of its own falls back to the general one.
  ['not_allowed', 'Check the highlighted fields.'],
];

/** 03 §7.2's password codes. */
export const PASSWORD_CODES: readonly FieldRow[] = [
  ['required', 'Enter a password.'],
  ['too_short', "That's too short for a password."],
  ['too_long', "That's too long for a password."],
  ['invalid', "That password has characters that can't be used."],
  ['too_common', "That password is too common. Pick one that's harder to guess."],
  ['same_as_username', "Your password can't be your username."],
];
