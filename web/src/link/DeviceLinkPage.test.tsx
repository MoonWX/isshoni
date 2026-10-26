// /link (docs/m2/02-device-auth.md §10.1, §16.3) against MSW, inside the app's routes with the real link folder:
// every state (enter, loading, confirm in its three network variants, linked, denied, invalid, rate_limited, error),
// ?code= normalized and taken out of the address bar, the "I started this myself" tick for every network but `same`,
// one decide per double click, the name the friend may change, focus and announcements, and the signed-out paths.
import { screen, waitFor, within } from '@testing-library/react';
import userEvent, { type UserEvent } from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { beforeEach, describe, expect, it } from 'vitest';

// Loaded with the test file rather than by the first test's navigation (see account/testing/harness.tsx).
import '../auth/index';
import './index';

import { createAppRoutes, type AppRouteSources } from '../app/router';
import { mockSession, NOT_THE_ANSWER, networkError, noContent, onPost, pageSources } from '../auth/testing/harness';
import type { DeviceLookup, Error as ApiErrorBody } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { apiError, apiPath, infoFixture, meFixture, server } from '../test/msw';
import { createTestServices, renderRoute } from '../test/render';

const LOGIN_HEADING = 'Log in to Test server';
const CONFIRM_HEADING = 'Link the isshoni app?';
const ENTER_HEADING = 'Enter the code shown in the isshoni app';
const MINUTE = 60_000;

/** The app's routes with the real auth and link folders. */
function linkSources(): AppRouteSources {
  const base = pageSources();
  return { ...base, lazy: { ...base.lazy, link: () => import('./index') } };
}

/** Renders /link (with its query) for a signed-in member, alex, on infoFixture()'s server. */
function renderLink(path: string, { signedIn = true }: { signedIn?: boolean } = {}) {
  const session = mockSession(signedIn ? meFixture() : null);
  const services = createTestServices({ info: infoFixture() });
  return { ...renderRoute({ path, routes: createAppRoutes(linkSources()), services }), session };
}

/** A lookup answer as the server sends it (02 §5.3), requested a minute ago from 203.0.113.7. */
function lookupFixture(overrides: Partial<DeviceLookup> = {}): DeviceLookup {
  const now = Date.now();
  return {
    userCode: 'WDJB-MJHT',
    clientKind: 'desktop',
    os: 'windows',
    deviceName: 'DESKTOP-7F3K2LQ',
    appVersion: '0.2.0',
    requestIp: '203.0.113.7',
    network: 'same',
    requestedAt: new Date(now - MINUTE).toISOString(),
    expiresAt: new Date(now + 9 * MINUTE).toISOString(),
    ...overrides,
  };
}

/** Installs POST /device/lookup answering respond(body); returns the recorded bodies. */
function onLookup(respond: (body: unknown) => Response | Promise<Response>): unknown[] {
  return onPost('/api/v1/device/lookup', respond);
}

function lookupAnswer(overrides: Partial<DeviceLookup> = {}): () => Response {
  return () => HttpResponse.json(lookupFixture(overrides));
}

async function confirmCard(): Promise<UserEvent> {
  await screen.findByRole('heading', { level: 1, name: CONFIRM_HEADING });
  return userEvent.setup();
}

function linkButton(name: 'Link' | 'Link anyway' = 'Link'): HTMLElement {
  return screen.getByRole('button', { name });
}

describe('DeviceLinkPage: the code from the link', () => {
  it('normalizes ?code=, takes it out of the address bar, and looks it up', async () => {
    const asked = onLookup(lookupAnswer());
    const { router } = renderLink('/link?code=wdjb%20mjht');
    await confirmCard();
    expect(asked).toEqual([{ userCode: 'WDJBMJHT' }]);
    expect(router.state.location.pathname).toBe('/link');
    expect(router.state.location.search).toBe('');
  });

  it('keeps the other query parameters', async () => {
    onLookup(lookupAnswer());
    const { router } = renderLink('/link?utm=x&code=WDJB-MJHT');
    await confirmCard();
    expect(router.state.location.search).toBe('?utm=x');
  });

  it('a malformed code shows the code field with what the link had, and asks nothing', async () => {
    const asked = onLookup(lookupAnswer());
    const { router } = renderLink('/link?code=wdjb-mjha');
    expect(await screen.findByRole('heading', { level: 1, name: ENTER_HEADING })).toBeInTheDocument();
    expect(screen.getByLabelText('Code')).toHaveValue('WDJB-MJHA');
    expect(screen.getByRole('button', { name: 'Continue' })).toBeDisabled();
    expect(router.state.location.search).toBe('');
    expect(asked).toEqual([]);
  });
});

describe('DeviceLinkPage: enter', () => {
  it('formats the field as the friend types, and Continue looks up 8 letters of the alphabet', async () => {
    const asked = onLookup(lookupAnswer());
    renderLink('/link');
    await screen.findByRole('heading', { level: 1, name: ENTER_HEADING });
    const user = userEvent.setup();
    const field = screen.getByLabelText('Code');
    expect(field).toHaveAttribute('autocapitalize', 'characters');
    expect(field).toHaveAccessibleDescription('8 letters, like WDJB-MJHT.');
    const go = screen.getByRole('button', { name: 'Continue' });
    expect(go).toBeDisabled();

    await user.type(field, 'wdjbmjh');
    expect(field).toHaveValue('WDJB-MJH');
    expect(go).toBeDisabled();
    // A vowel isn't in the alphabet: still off.
    await user.type(field, 'a');
    expect(field).toHaveValue('WDJB-MJHA');
    expect(go).toBeDisabled();
    await user.type(field, '{Backspace}t');
    expect(field).toHaveValue('WDJB-MJHT');
    expect(go).toBeEnabled();
    await user.click(go);

    await confirmCard();
    expect(asked).toEqual([{ userCode: 'WDJBMJHT' }]);
    expect(screen.getByRole('heading', { level: 1, name: CONFIRM_HEADING })).toHaveFocus();
  });

  it('takes a pasted code with spaces and lower case', async () => {
    const asked = onLookup(lookupAnswer());
    renderLink('/link');
    await screen.findByRole('heading', { level: 1, name: ENTER_HEADING });
    const user = userEvent.setup();
    await user.click(screen.getByLabelText('Code'));
    await user.paste(' wdjb mjht ');
    expect(screen.getByLabelText('Code')).toHaveValue('WDJB-MJHT');
    await user.keyboard('{Enter}');
    await confirmCard();
    expect(asked).toEqual([{ userCode: 'WDJBMJHT' }]);
  });
});

describe('DeviceLinkPage: confirm', () => {
  it('network same: the code to compare, the app, the name, where from, what the app may do, and no tick', async () => {
    onLookup(lookupAnswer());
    renderLink('/link?code=WDJB-MJHT');
    await confirmCard();
    // The code in large type, read letter by letter.
    const spoken = screen.getByText('W D J B, M J H T');
    const code = spoken.closest('p');
    expect(code).toHaveTextContent('WDJB-MJHT');
    expect(within(code ?? document.body).getByText('WDJB-MJHT')).toHaveAttribute('aria-hidden', 'true');
    expect(screen.getByText('Make sure it matches the code in the app.')).toBeInTheDocument();
    expect(screen.getByText('isshoni for Windows 0.2.0')).toBeInTheDocument();
    const name = screen.getByLabelText('Name for this computer');
    expect(name).toHaveValue('DESKTOP-7F3K2LQ');
    expect(name).toHaveAccessibleDescription('You can rename it later on your Devices page.');
    expect(
      screen.getByText(
        "Requested 1 minute ago from 203.0.113.7 (this browser's network). People on the same Wi-Fi or mobile carrier can share one address, so compare the code too.",
      ),
    ).toBeInTheDocument();
    const actsAs = screen.getByText(/^The app will act as/);
    expect(actsAs).toHaveTextContent('The app will act as alex on Test server: watch, share and change your settings.');
    expect(within(actsAs).getByText('alex').tagName).toBe('STRONG');
    expect(screen.getByText('Signed in as alex. Not you?')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Sign out' })).toBeInTheDocument();

    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument();
    expect(screen.queryByText(/different network/)).not.toBeInTheDocument();
    expect(linkButton()).toBeEnabled();
    expect(screen.getByRole('button', { name: "Don't link" })).toBeEnabled();
  });

  it('network different (both IPv4, other addresses): the warning, and Link anyway only after the tick', async () => {
    onLookup(lookupAnswer({ network: 'different', requestIp: '198.51.100.23' }));
    renderLink('/link?code=WDJB-MJHT');
    const user = await confirmCard();
    expect(screen.getByText('Requested 1 minute ago from 198.51.100.23.')).toBeInTheDocument();
    expect(screen.queryByText(/this browser's network/)).not.toBeInTheDocument();
    const warning =
      'This request came from a different network than this browser. Only continue if you started it yourself, for example on another computer or on mobile data.';
    expect(screen.getByText(warning)).toBeInTheDocument();
    const tick = screen.getByRole('checkbox', { name: 'I started this myself' });
    expect(tick).toHaveAccessibleDescription(warning);
    expect(tick).not.toBeChecked();
    expect(screen.queryByRole('button', { name: 'Link' })).not.toBeInTheDocument();
    expect(linkButton('Link anyway')).toBeDisabled();

    await user.click(tick);
    expect(linkButton('Link anyway')).toBeEnabled();
    await user.click(tick);
    expect(linkButton('Link anyway')).toBeDisabled();
  });

  it('network unknown (the app over IPv6, the browser over IPv4): the softer note, and Link only after the tick', async () => {
    onLookup(lookupAnswer({ network: 'unknown', requestIp: '2001:db8::7' }));
    renderLink('/link?code=WDJB-MJHT');
    const user = await confirmCard();
    expect(screen.getByText('Requested 1 minute ago from 2001:db8::7.')).toBeInTheDocument();
    const note =
      "isshoni couldn't check whether this request came from your network. Only continue if you started it yourself.";
    expect(screen.getByText(note)).toBeInTheDocument();
    expect(screen.queryByText(/different network/)).not.toBeInTheDocument();
    const tick = screen.getByRole('checkbox', { name: 'I started this myself' });
    expect(tick).toHaveAccessibleDescription(note);
    // Link keeps its label.
    expect(linkButton()).toBeDisabled();
    await user.click(tick);
    expect(linkButton()).toBeEnabled();
  });

  it('a network value this page doesn’t know fails closed: the unknown note and the tick', async () => {
    onLookup(() => HttpResponse.json({ ...lookupFixture(), network: 'nearby' }));
    renderLink('/link?code=WDJB-MJHT');
    await confirmCard();
    expect(screen.getByText(/^isshoni couldn't check whether this request came from your network/)).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'I started this myself' })).toBeInTheDocument();
    expect(linkButton()).toBeDisabled();
  });

  it('names a phone and an app this page doesn’t know', async () => {
    onLookup(lookupAnswer({ clientKind: 'mobile', os: 'android', appVersion: '1.0.0' }));
    renderLink('/link?code=WDJB-MJHT');
    await confirmCard();
    expect(screen.getByText('isshoni for Android 1.0.0')).toBeInTheDocument();
    expect(screen.getByLabelText('Name for this device')).toBeInTheDocument();
  });

  it('a request time ahead of this browser’s clock reads "now"', async () => {
    onLookup(lookupAnswer({ requestedAt: new Date(Date.now() + 30_000).toISOString() }));
    renderLink('/link?code=WDJB-MJHT');
    await confirmCard();
    expect(screen.getByText(/^Requested now from 203\.0\.113\.7/)).toBeInTheDocument();
  });
});

describe('DeviceLinkPage: deciding', () => {
  it('Link approves with the app’s name kept: linked, focused and announced', async () => {
    onLookup(lookupAnswer());
    const decided = onPost('/api/v1/device/decide', noContent);
    const { services } = renderLink('/link?code=WDJB-MJHT');
    const user = await confirmCard();
    await user.click(linkButton());
    const heading = await screen.findByRole('heading', { level: 1, name: 'Linked' });
    expect(screen.getByText('Go back to the app; it continues by itself.')).toBeInTheDocument();
    expect(heading).toHaveFocus();
    expect(services.ui.getState().announcements.polite?.text).toBe('Go back to the app; it continues by itself.');
    // No deviceName: the app's own proposal stays.
    expect(decided).toEqual([{ userCode: 'WDJBMJHT', approve: true }]);
  });

  it('sends the name the friend typed, trimmed', async () => {
    onLookup(lookupAnswer());
    const decided = onPost('/api/v1/device/decide', noContent);
    renderLink('/link?code=WDJB-MJHT');
    const user = await confirmCard();
    const name = screen.getByLabelText('Name for this computer');
    await user.clear(name);
    await user.type(name, '  Gaming PC  ');
    await user.click(linkButton());
    await screen.findByRole('heading', { level: 1, name: 'Linked' });
    expect(decided).toEqual([{ userCode: 'WDJBMJHT', approve: true, deviceName: 'Gaming PC' }]);
  });

  it('after the tick, Link anyway approves', async () => {
    onLookup(lookupAnswer({ network: 'different' }));
    const decided = onPost('/api/v1/device/decide', noContent);
    renderLink('/link?code=WDJB-MJHT');
    const user = await confirmCard();
    await user.click(screen.getByRole('checkbox', { name: 'I started this myself' }));
    await user.click(linkButton('Link anyway'));
    await screen.findByRole('heading', { level: 1, name: 'Linked' });
    expect(decided).toEqual([{ userCode: 'WDJBMJHT', approve: true }]);
  });

  it("Don't link denies: not linked, focused and announced", async () => {
    onLookup(lookupAnswer({ network: 'unknown' }));
    const decided = onPost('/api/v1/device/decide', noContent);
    const { services } = renderLink('/link?code=WDJB-MJHT');
    const user = await confirmCard();
    // Saying no needs no tick.
    await user.click(screen.getByRole('button', { name: "Don't link" }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Not linked' })).toHaveFocus();
    expect(screen.getByText('The app will show that you said no.')).toBeInTheDocument();
    expect(services.ui.getState().announcements.polite?.text).toBe('The app will show that you said no.');
    expect(decided).toEqual([{ userCode: 'WDJBMJHT', approve: false }]);
  });

  it('a double click sends one decide', async () => {
    onLookup(lookupAnswer());
    let release: () => void = () => undefined;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    const decided = onPost('/api/v1/device/decide', async () => {
      await held;
      return noContent();
    });
    renderLink('/link?code=WDJB-MJHT');
    const user = await confirmCard();
    await user.dblClick(linkButton());
    await user.click(screen.getByRole('button', { name: "Don't link" }));
    release();
    await screen.findByRole('heading', { level: 1, name: 'Linked' });
    expect(decided).toEqual([{ userCode: 'WDJBMJHT', approve: true }]);
  });

  it('checks the name before it sends anything', async () => {
    onLookup(lookupAnswer());
    const decided = onPost('/api/v1/device/decide', noContent);
    renderLink('/link?code=WDJB-MJHT');
    const user = await confirmCard();
    const name = screen.getByLabelText('Name for this computer');
    await user.clear(name);
    await user.type(name, '   ');
    await user.click(linkButton());
    expect(name).toHaveAccessibleDescription('You can rename it later on your Devices page. Enter a name.');
    expect(name).toHaveAttribute('aria-invalid', 'true');
    expect(name).toHaveFocus();

    await user.clear(name);
    await user.type(name, 'x'.repeat(65));
    await user.click(linkButton());
    expect(name).toHaveAccessibleDescription(/That's too long for a name: 64 characters at most\.$/);
    expect(decided).toEqual([]);
  });

  it('shows a name the server refused (422 {deviceName}) under the field, and Link works again', async () => {
    onLookup(lookupAnswer());
    let refuse = true;
    const decided = onPost('/api/v1/device/decide', () =>
      refuse ? apiError(422, { code: 'validation_failed', fields: { deviceName: 'invalid' } }) : noContent(),
    );
    renderLink('/link?code=WDJB-MJHT');
    const user = await confirmCard();
    const name = screen.getByLabelText('Name for this computer');
    await user.clear(name);
    await user.type(name, 'Bad​Name');
    await user.click(linkButton());
    await waitFor(() => {
      expect(name).toHaveAccessibleDescription(/That name has characters that can't be used\.$/);
    });
    expect(name).toHaveFocus();

    refuse = false;
    await user.clear(name);
    await user.type(name, 'Good');
    await user.click(linkButton());
    await screen.findByRole('heading', { level: 1, name: 'Linked' });
    expect(decided).toHaveLength(2);
  });

  it.each([
    ['the server fails', () => apiError(500, { code: 'internal', requestId: 'req-7f3a' }), /Reference: req-7f3a$/],
    ['there is no connection', networkError, /You're offline\. Check your connection\.$/],
  ])('keeps the card and says why when %s; Link tries again', async (_name, respond, reason) => {
    onLookup(lookupAnswer());
    let fail = true;
    const decided = onPost('/api/v1/device/decide', () => (fail ? respond() : noContent()));
    renderLink('/link?code=WDJB-MJHT');
    const user = await confirmCard();
    await user.click(linkButton());
    expect(await screen.findByRole('alert')).toHaveTextContent(reason);
    expect(screen.getByRole('heading', { level: 1, name: CONFIRM_HEADING })).toBeInTheDocument();
    fail = false;
    await user.click(linkButton());
    await screen.findByRole('heading', { level: 1, name: 'Linked' });
    expect(decided).toHaveLength(2);
  });

  it('a code that expired meanwhile (404 on decide) shows invalid', async () => {
    onLookup(lookupAnswer());
    onPost('/api/v1/device/decide', () => apiError(404, { code: 'device_code_invalid' }));
    renderLink('/link?code=WDJB-MJHT');
    const user = await confirmCard();
    await user.click(linkButton());
    expect(await screen.findByRole('heading', { level: 1, name: "This code doesn't work" })).toHaveFocus();
  });
});

describe('DeviceLinkPage: the lookup’s other answers', () => {
  it('404 device_code_invalid: invalid, announced, with the code field for another code', async () => {
    let known = false;
    const asked = onLookup(() =>
      known ? HttpResponse.json(lookupFixture()) : apiError(404, { code: 'device_code_invalid' }),
    );
    const { services } = renderLink('/link?code=BCDF-GHJK');
    const heading = await screen.findByRole('heading', { level: 1, name: "This code doesn't work" });
    const text = 'This code is wrong, was already used or has expired. Start again in the app.';
    expect(screen.getByText(text)).toBeInTheDocument();
    expect(heading).toHaveFocus();
    expect(services.ui.getState().announcements.polite?.text).toBe(text);

    const user = userEvent.setup();
    const field = screen.getByLabelText('Code');
    expect(field).toHaveValue('');
    known = true;
    await user.type(field, 'wdjbmjht{Enter}');
    await confirmCard();
    expect(asked).toEqual([{ userCode: 'BCDFGHJK' }, { userCode: 'WDJBMJHT' }]);
  });

  it('429: too many wrong codes, with the wait in minutes', async () => {
    onLookup(() => apiError(429, { code: 'rate_limited', retryAfter: 90 }));
    const { services } = renderLink('/link?code=WDJB-MJHT');
    expect(await screen.findByRole('heading', { level: 1, name: 'Too many wrong codes' })).toHaveFocus();
    expect(screen.getByText('Try again in 2 min.')).toBeInTheDocument();
    expect(services.ui.getState().announcements.polite?.text).toBe('Try again in 2 min.');
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
  });

  it('429 on decide shows the wait too', async () => {
    onLookup(lookupAnswer());
    onPost('/api/v1/device/decide', () => apiError(429, { code: 'rate_limited', retryAfter: 30 }));
    renderLink('/link?code=WDJB-MJHT');
    const user = await confirmCard();
    await user.click(linkButton());
    await screen.findByRole('heading', { level: 1, name: 'Too many wrong codes' });
    expect(screen.getByText('Try again in 1 min.')).toBeInTheDocument();
  });

  const failures: readonly (readonly [string, () => Response, RegExp])[] = [
    ['a server error', () => apiError(500, { code: 'internal', requestId: 'req-7f3a' }), /Reference: req-7f3a$/],
    ['no connection', networkError, /You're offline\. Check your connection\.$/],
    ...NOT_THE_ANSWER.map(
      ([name, respond]) => [`2xx: ${name}`, respond, /Something went wrong \(unknown\)\./] as const,
    ),
  ];

  it.each(failures)('%s: the reason with Try again, which looks the code up again', async (_name, respond, reason) => {
    let fail = true;
    const asked = onLookup(() => (fail ? respond() : HttpResponse.json(lookupFixture())));
    renderLink('/link?code=WDJB-MJHT');
    expect(await screen.findByRole('alert')).toHaveTextContent(reason);
    expect(screen.getByRole('heading', { level: 1, name: 'Link the isshoni app' })).toBeInTheDocument();
    fail = false;
    await userEvent.setup().click(screen.getByRole('button', { name: 'Try again' }));
    await confirmCard();
    expect(asked).toEqual([{ userCode: 'WDJBMJHT' }, { userCode: 'WDJBMJHT' }]);
  });

  it('shows a spinner while it looks the code up', async () => {
    let release: () => void = () => undefined;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    onLookup(async () => {
      await held;
      return HttpResponse.json(lookupFixture());
    });
    renderLink('/link?code=WDJB-MJHT');
    expect(await screen.findByRole('status', { name: '' })).toHaveTextContent('Checking the code');
    expect(screen.getByRole('heading', { level: 1, name: 'Link the isshoni app' })).toBeInTheDocument();
    release();
    await confirmCard();
  });
});

describe('DeviceLinkPage: signed out', () => {
  it('a signed-out browser goes to the login page with the code in next=, and comes back to the card', async () => {
    const asked = onLookup(lookupAnswer());
    const { router, session } = renderLink('/link?code=WDJB-MJHT', { signedIn: false });
    await screen.findByRole('heading', { level: 1, name: LOGIN_HEADING });
    expect(router.state.location.pathname + router.state.location.search).toBe(
      '/login?next=%2Flink%3Fcode%3DWDJB-MJHT',
    );
    expect(asked).toEqual([]);

    onPost('/api/v1/auth/login', () => {
      session.me = meFixture();
      return HttpResponse.json({ user: meFixture().user });
    });
    const user = userEvent.setup();
    await user.type(screen.getByLabelText('Username'), 'alex');
    await user.type(screen.getByLabelText('Password'), 'correct horse battery');
    await user.click(screen.getByRole('button', { name: 'Log in' }));
    await confirmCard();
    expect(asked).toEqual([{ userCode: 'WDJBMJHT' }]);
    expect(router.state.location.search).toBe('');
  });

  it('"Not you? Sign out" logs out and keeps the code for the next login', async () => {
    onLookup(lookupAnswer());
    const { router, session, services } = renderLink('/link?code=WDJB-MJHT');
    const loggedOut = onPost('/api/v1/auth/logout', () => {
      session.me = null;
      return noContent();
    });
    const user = await confirmCard();
    await user.click(screen.getByRole('button', { name: 'Sign out' }));
    await screen.findByRole('heading', { level: 1, name: LOGIN_HEADING });
    expect(loggedOut).toEqual([{}]);
    expect(services.queryClient.getQueryData(queryKeys.me)).toBeNull();
    expect(router.state.location.pathname + router.state.location.search).toBe(
      '/login?next=%2Flink%3Fcode%3DWDJB-MJHT',
    );
  });

  it('a sign-out that cannot reach the server keeps the card and takes the code out of the address bar again', async () => {
    onLookup(lookupAnswer());
    const { router, services } = renderLink('/link?code=WDJB-MJHT');
    onPost('/api/v1/auth/logout', networkError);
    const user = await confirmCard();
    await user.click(screen.getByRole('button', { name: 'Sign out' }));
    await waitFor(() => {
      expect(services.ui.getState().toasts).toHaveLength(1);
    });
    expect(screen.getByRole('heading', { level: 1, name: CONFIRM_HEADING })).toBeInTheDocument();
    await waitFor(() => {
      expect(router.state.location.search).toBe('');
    });
  });

  it.each<readonly [string, string, ApiErrorBody]>([
    ['the lookup', 'lookup', { code: 'unauthenticated' }],
    ['the decide', 'decide', { code: 'unauthenticated' }],
  ])('a session that ended under %s (401) goes to the login page with the code', async (_name, which, body) => {
    onLookup(which === 'lookup' ? () => apiError(401, body) : lookupAnswer());
    onPost('/api/v1/device/decide', () => apiError(401, body));
    const { router, session } = renderLink('/link?code=WDJB-MJHT');
    if (which === 'decide') {
      const user = await confirmCard();
      session.me = null;
      await user.click(linkButton());
    } else {
      session.me = null;
    }
    await screen.findByRole('heading', { level: 1, name: LOGIN_HEADING });
    expect(router.state.location.pathname + router.state.location.search).toBe(
      '/login?next=%2Flink%3Fcode%3DWDJB-MJHT',
    );
  });
});

describe('DeviceLinkPage: no GET approves', () => {
  let gets: string[];

  beforeEach(() => {
    gets = [];
    server.use(
      http.get(apiPath('/api/v1/device/*'), ({ request }) => {
        gets.push(new URL(request.url).pathname);
        return apiError(405, { code: 'method_not_allowed' });
      }),
    );
  });

  it('opening the page with a code only looks it up (POST); approval is the Link button’s POST', async () => {
    const asked = onLookup(lookupAnswer());
    const decided = onPost('/api/v1/device/decide', noContent);
    renderLink('/link?code=WDJB-MJHT');
    await confirmCard();
    expect(asked).toHaveLength(1);
    expect(decided).toEqual([]);
    expect(gets).toEqual([]);
  });
});
