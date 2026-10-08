// /setup#<token>, step 1 of the wizard (05 §14.1, 03 §7.8) against MSW: the token, setup/check (#7), the
// create-admin form, setup/complete (#8) with every error code, and where the page goes afterwards.
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import { fragmentTokenKey } from '../app/boot';
import {
  COMMON_ERRORS,
  fillAccount,
  mockSession,
  networkError,
  noContent,
  onPost,
  PASSWORD_CODES,
  renderPage,
  ROOM_HEADING,
  typeInto,
  USERNAME_CODES,
  type ErrorRow,
  type SessionState,
} from '../auth/testing/harness';
import { queryKeys } from '../protocol/queryKeys';
import { apiError, apiPath, infoFixture, meFixture, server } from '../test/msw';
import { AFTER_SETUP_PATH } from './SetupPage';

const TOKEN = 'EXAMPLEsetupTOKEN0123456789abcdef';
const HEADING = 'Create the admin account';
const ALREADY = 'This server is already set up';
const INVALID = "This setup link doesn't work anymore";

/** A fresh server: no admin yet, and its name is still its host. */
const FRESH = { setupRequired: true, server: { name: 'watch.example.com', version: '0.1.0', publicUrl: '' } };

function checkWorks(): unknown[] {
  return onPost('/api/v1/auth/setup/check', noContent);
}

/** POST /api/v1/auth/setup/complete that creates the admin and starts the session. */
function completeWorks(session: SessionState): unknown[] {
  return onPost('/api/v1/auth/setup/complete', () => {
    session.me = meFixture({ admin: true });
    return HttpResponse.json({ user: { id: 'a1b2c3d4e5f6', username: 'Alex', role: 'admin' } }, { status: 201 });
  });
}

function renderSetup() {
  return renderPage({ path: '/setup', token: ['setup', TOKEN], info: FRESH });
}

async function submitAdmin({ username = 'Alex', password = 'correct horse battery', serverName = '' } = {}) {
  const user = userEvent.setup();
  await screen.findByLabelText('Server name');
  await fillAccount(user, username, password);
  await typeInto(user, 'Server name', serverName);
  await user.click(screen.getByRole('button', { name: 'Create account' }));
  return user;
}

describe('SetupPage', () => {
  it('reads the stashed token, checks it, creates the admin and goes to "/"', async () => {
    const session = mockSession();
    const checked = checkWorks();
    const completed = completeWorks(session);
    const { router, platform, services } = renderSetup();
    // The token boot step 0 took from /setup#<token>.
    expect(platform.storage.session.get(fragmentTokenKey('setup'))).toBe(TOKEN);

    expect(await screen.findByRole('heading', { name: HEADING })).toBeInTheDocument();
    await screen.findByLabelText('Server name');
    expect(checked).toEqual([{ token: TOKEN }]);

    // After the admin exists the server reports itself as set up, under its new name.
    server.use(
      http.get(apiPath('/api/v1/info'), () =>
        HttpResponse.json(
          infoFixture({ server: { name: "Alex's server", version: '0.1.0', publicUrl: '' }, setupRequired: false }),
        ),
      ),
    );
    await submitAdmin({ serverName: "  Alex's server " });

    expect(await screen.findByRole('heading', { name: ROOM_HEADING })).toBeInTheDocument();
    // Until the wizard's steps 2–3 exist (S87: /admin/welcome?step=2).
    expect(AFTER_SETUP_PATH).toBe('/');
    expect(router.state.location.pathname + router.state.location.search).toBe('/');
    expect(completed).toEqual([
      { token: TOKEN, username: 'Alex', password: 'correct horse battery', serverName: "Alex's server" },
    ]);
    // The stored token is cleared, and the check didn't run a second time.
    expect(platform.storage.session.get(fragmentTokenKey('setup'))).toBeNull();
    expect(checked).toHaveLength(1);
    // /info is read again: the rest of the app sees the new name.
    await waitFor(() => {
      expect(services.queryClient.getQueryData(queryKeys.info)).toMatchObject({
        setupRequired: false,
        server: { name: "Alex's server" },
      });
    });
  });

  it('leaves the server name out when the field is empty', async () => {
    const session = mockSession();
    checkWorks();
    const completed = completeWorks(session);
    renderSetup();
    await submitAdmin();
    expect(await screen.findByRole('heading', { name: ROOM_HEADING })).toBeInTheDocument();
    expect(completed).toEqual([{ token: TOKEN, username: 'Alex', password: 'correct horse battery' }]);
  });

  it('shows the form with the rules of /info, a password without a repeat field, and all three steps', async () => {
    mockSession();
    checkWorks();
    renderSetup();
    await screen.findByLabelText('Server name');
    expect(screen.getByLabelText('Username')).toHaveAccessibleDescription(/^2 to 32 characters/);
    const password = screen.getByLabelText('Password');
    expect(password).toHaveAccessibleDescription(/^At least 8 characters/);
    expect(password).toHaveAttribute('type', 'password');
    await userEvent.click(screen.getByRole('button', { name: 'Show password' }));
    expect(password).toHaveAttribute('type', 'text');
    expect(screen.getAllByLabelText(/password/i).filter((el) => el.tagName === 'INPUT')).toHaveLength(1);
    // Optional, and empty means the server's host.
    expect(screen.getByLabelText('Server name')).not.toBeRequired();
    expect(screen.getByLabelText('Server name')).toHaveAttribute('placeholder', 'watch.example.com');
    expect(screen.getByLabelText('Server name')).toHaveAttribute('maxlength', '64');

    const steps = within(screen.getByRole('list', { name: 'Setup' })).getAllByRole('listitem');
    // Each step: its number (decorative), then what screen readers hear.
    expect(steps.map((li) => li.textContent)).toEqual([
      '1Step 1 of 3: Create the admin',
      '2Step 2 of 3: Connection test',
      '3Step 3 of 3: Invite friends',
    ]);
    expect(steps[0]).toHaveAttribute('aria-current', 'step');
    expect(steps[1]).not.toHaveAttribute('aria-current');
  });

  describe('without a token', () => {
    it('on a server that waits for its admin: the commands that print the link', async () => {
      mockSession();
      const checked = checkWorks();
      renderPage({ path: '/setup', info: FRESH });
      expect(await screen.findByRole('heading', { name: 'Open the setup link' })).toBeInTheDocument();
      expect(screen.getByText('sudo isshoni setup-url')).toBeInTheDocument();
      expect(screen.getByText('docker compose exec isshoni isshoni setup-url')).toBeInTheDocument();
      expect(checked).toEqual([]);
    });

    it('on a server that has its admin: already set up → Log in', async () => {
      mockSession();
      renderPage({ path: '/setup' });
      expect(await screen.findByRole('heading', { name: ALREADY })).toBeInTheDocument();
      expect(screen.getByRole('link', { name: 'Log in' })).toHaveAttribute('href', '/login');
    });
  });

  describe('error codes of POST /api/v1/auth/setup/check', () => {
    it('setup_token_invalid: replaced or expired, with the command for a new link', async () => {
      mockSession();
      onPost('/api/v1/auth/setup/check', () => apiError(404, { code: 'setup_token_invalid' }));
      renderSetup();
      expect(await screen.findByRole('heading', { name: INVALID })).toBeInTheDocument();
      expect(screen.getByText(/It was replaced or has expired/)).toBeInTheDocument();
      expect(screen.getByText('sudo isshoni setup-url')).toBeInTheDocument();
      expect(screen.getByText('docker compose exec isshoni isshoni setup-url')).toBeInTheDocument();
      expect(screen.queryByLabelText('Username')).not.toBeInTheDocument();
    });

    it('setup_unavailable: already set up → Log in', async () => {
      mockSession();
      onPost('/api/v1/auth/setup/check', () => apiError(404, { code: 'setup_unavailable' }));
      renderSetup();
      expect(await screen.findByRole('heading', { name: ALREADY })).toBeInTheDocument();
      expect(screen.getByRole('link', { name: 'Log in' })).toHaveAttribute('href', '/login');
      expect(screen.queryByLabelText('Username')).not.toBeInTheDocument();
    });

    it.each([
      ['offline', networkError, "You're offline. Check your connection."],
      [
        'rate_limited',
        () => apiError(429, { code: 'rate_limited', retryAfter: 15 }),
        'Too many attempts. Try again in 15 seconds.',
      ],
      [
        'internal',
        () => apiError(500, { code: 'internal', requestId: 'req-3' }),
        'Something went wrong on the server. Reference: req-3',
      ],
    ])('%s: the check could not finish; "Try again" asks once more', async (_name, respond, shown) => {
      mockSession();
      let failing = true;
      const checked = onPost('/api/v1/auth/setup/check', () => (failing ? respond() : noContent()));
      renderSetup();
      expect(await screen.findByRole('alert')).toHaveTextContent(shown);
      failing = false;
      await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
      expect(await screen.findByLabelText('Server name')).toBeInTheDocument();
      expect(checked).toHaveLength(2);
    });
  });

  describe('error codes of POST /api/v1/auth/setup/complete', () => {
    it('setup_unavailable: another tab created the admin first → Log in', async () => {
      mockSession();
      checkWorks();
      onPost('/api/v1/auth/setup/complete', () => apiError(404, { code: 'setup_unavailable' }));
      renderSetup();
      await submitAdmin();
      const heading = await screen.findByRole('heading', { name: ALREADY });
      expect(screen.getByRole('link', { name: 'Log in' })).toHaveAttribute('href', '/login');
      // The button that had focus went with the form: focus moves to the new heading, so the change is heard.
      expect(heading).toBe(screen.getByRole('heading', { level: 1 }));
      await waitFor(() => {
        expect(heading).toHaveFocus();
      });
    });

    it('setup_token_invalid: a newer link replaced this one in the meantime', async () => {
      mockSession();
      checkWorks();
      onPost('/api/v1/auth/setup/complete', () => apiError(404, { code: 'setup_token_invalid' }));
      renderSetup();
      await submitAdmin();
      const heading = await screen.findByRole('heading', { name: INVALID });
      expect(screen.getByText('sudo isshoni setup-url')).toBeInTheDocument();
      expect(heading).toBe(screen.getByRole('heading', { level: 1 }));
      await waitFor(() => {
        expect(heading).toHaveFocus();
      });
    });

    it.each([
      ...USERNAME_CODES.map(([code, shown]) => ['username', 'Username', code, shown] as const),
      ...PASSWORD_CODES.map(([code, shown]) => ['password', 'Password', code, shown] as const),
      ['serverName', 'Server name', 'too_long', "That's too long for a name."] as const,
      ['serverName', 'Server name', 'invalid', "That name has characters that can't be used."] as const,
    ])('validation_failed, %s %s', async (field, label, code, shown) => {
      mockSession();
      checkWorks();
      onPost('/api/v1/auth/setup/complete', () =>
        apiError(422, { code: 'validation_failed', fields: { [field]: code } }),
      );
      renderSetup();
      await submitAdmin({ serverName: 'Movie night' });
      const input = screen.getByLabelText(label);
      await waitFor(() => {
        expect(input).toBeInvalid();
      });
      expect(screen.getByText(shown)).toBeInTheDocument();
      expect(input).toHaveFocus();
    });

    it.each<ErrorRow>([
      ['rate_limited', 429, { code: 'rate_limited', retryAfter: 42 }, 'Too many attempts. Try again in 42 seconds.'],
      ['server_busy', 503, { code: 'server_busy', retryAfter: 5 }, 'The server is busy. Try again in 5 seconds.'],
      ...COMMON_ERRORS,
    ])('%s', async (_name, status, error, shown) => {
      mockSession();
      checkWorks();
      onPost('/api/v1/auth/setup/complete', () => apiError(status, error));
      const { router, platform } = renderSetup();
      await submitAdmin();
      expect(await screen.findByRole('alert')).toHaveTextContent(shown);
      expect(router.state.location.pathname).toBe('/setup');
      // Nothing was created: the token stays for the next try.
      expect(platform.storage.session.get(fragmentTokenKey('setup'))).toBe(TOKEN);
    });
  });

  it.each([
    [{ username: '' }, 'Username', 'Enter a username.'],
    [{ username: 'a' }, 'Username', "That's too short for a username."],
    [{ password: 'short' }, 'Password', "That's too short for a password."],
  ])('checks the rules before it sends anything: %j', async (values, label, shown) => {
    mockSession();
    checkWorks();
    const completed = onPost('/api/v1/auth/setup/complete', () => apiError(500, { code: 'internal' }));
    renderSetup();
    await submitAdmin(values);
    expect(screen.getByText(shown)).toBeInTheDocument();
    expect(screen.getByLabelText(label)).toHaveFocus();
    expect(completed).toEqual([]);
  });
});
