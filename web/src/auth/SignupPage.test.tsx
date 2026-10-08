// /signup and /pending (05 §15.1, 03 §7.9) against MSW: approval mode's request form and every error code of
// register (#5) without an invite.
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import { apiError, meFixture } from '../test/msw';
import {
  COMMON_ERRORS,
  fillAccount,
  mockSession,
  onPost,
  PASSWORD_CODES,
  renderPage,
  ROOM_HEADING,
  USERNAME_CODES,
  type ErrorRow,
} from './testing/harness';

const HEADING = 'Request an account';

function renderSignup() {
  return renderPage({ path: '/signup', info: { registration: 'approval' } });
}

async function submitRequest(username?: string, password?: string) {
  const user = userEvent.setup();
  await screen.findByRole('heading', { name: HEADING });
  await fillAccount(user, username, password);
  await user.click(screen.getByRole('button', { name: 'Send request' }));
  return user;
}

describe('SignupPage', () => {
  it('approval mode: sends the request without a token and goes to /pending', async () => {
    const session = mockSession();
    const registered = onPost('/api/v1/auth/register', () => HttpResponse.json({ status: 'pending' }, { status: 202 }));
    const { router } = renderSignup();
    expect(await screen.findByText(/An admin of Test server reviews every request/)).toBeInTheDocument();
    await submitRequest('sam_k', 'another long passphrase');
    expect(await screen.findByRole('heading', { name: 'Request sent' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/pending');
    expect(registered).toEqual([{ username: 'sam_k', password: 'another long passphrase' }]);
    // Pending users get no session (03 §7.9): nothing asks who is signed in.
    expect(session.meCalls).toBe(0);
  });

  it.each(['invite', 'closed'] as const)('redirects to /login in %s mode', async (registration) => {
    mockSession();
    const { router } = renderPage({ path: '/signup', info: { registration } });
    expect(await screen.findByRole('heading', { name: 'Log in to Test server' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/login');
  });

  it('an account that is active at once comes with a session: on to the room', async () => {
    const session = mockSession();
    onPost('/api/v1/auth/register', () => {
      session.me = meFixture();
      return HttpResponse.json(
        { status: 'active', user: { id: 'k3m9p2qxw7ht', username: 'alex', role: 'user' } },
        { status: 201 },
      );
    });
    renderSignup();
    await submitRequest();
    expect(await screen.findByRole('heading', { name: ROOM_HEADING })).toBeInTheDocument();
  });

  describe('error codes of POST /api/v1/auth/register', () => {
    it.each<ErrorRow>([
      ['invite_required', 403, { code: 'invite_required' }, 'You need an invite link to join this server.'],
      ['registration_closed', 403, { code: 'registration_closed' }, "This server isn't taking new accounts."],
      [
        'limit_reached (pending_signups)',
        409,
        { code: 'limit_reached', params: { limit: 'pending_signups' } },
        'Too many sign-ups are waiting for approval. Try again later.',
      ],
      [
        'rate_limited (5 sign-ups per hour and address)',
        429,
        { code: 'rate_limited', retryAfter: 720 },
        'Too many attempts. Try again in 720 seconds.',
      ],
      ['server_busy', 503, { code: 'server_busy', retryAfter: 5 }, 'The server is busy. Try again in 5 seconds.'],
      ...COMMON_ERRORS,
    ])('%s', async (_name, status, error, shown) => {
      mockSession();
      onPost('/api/v1/auth/register', () => apiError(status, error));
      const { router } = renderSignup();
      await submitRequest();
      expect(await screen.findByRole('alert')).toHaveTextContent(shown);
      expect(router.state.location.pathname).toBe('/signup');
    });

    it('username_taken: under the username (a pending request holds the name too)', async () => {
      mockSession();
      onPost('/api/v1/auth/register', () => apiError(409, { code: 'username_taken' }));
      renderSignup();
      await submitRequest();
      const username = screen.getByLabelText('Username');
      await waitFor(() => {
        expect(username).toBeInvalid();
      });
      expect(username).toHaveAccessibleDescription(/That username is taken\.$/);
    });

    it.each([
      ...USERNAME_CODES.map(([code, shown]) => ['username', 'Username', code, shown] as const),
      ...PASSWORD_CODES.map(([code, shown]) => ['password', 'Password', code, shown] as const),
    ])('validation_failed, %s %s', async (field, label, code, shown) => {
      mockSession();
      onPost('/api/v1/auth/register', () => apiError(422, { code: 'validation_failed', fields: { [field]: code } }));
      renderSignup();
      await submitRequest();
      const input = screen.getByLabelText(label);
      await waitFor(() => {
        expect(input).toBeInvalid();
      });
      expect(input.getAttribute('aria-describedby')).not.toBeNull();
      expect(screen.getByText(shown)).toBeInTheDocument();
      expect(input).toHaveFocus();
    });
  });

  it('checks the account rules before it sends anything', async () => {
    mockSession();
    const registered = onPost('/api/v1/auth/register', () => apiError(500, { code: 'internal' }));
    renderSignup();
    await submitRequest('a', 'short');
    expect(screen.getByText("That's too short for a username.")).toBeInTheDocument();
    expect(screen.getByText("That's too short for a password.")).toBeInTheDocument();
    expect(screen.getByLabelText('Username')).toHaveFocus();
    expect(registered).toEqual([]);
  });
});

describe('PendingPage', () => {
  it('is static: what happens next, and the way back to the login page', async () => {
    const session = mockSession();
    renderPage({ path: '/pending' });
    expect(await screen.findByRole('heading', { name: 'Request sent' })).toBeInTheDocument();
    expect(screen.getByText('An admin will review your request. Try logging in later.')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to log in' })).toHaveAttribute('href', '/login');
    expect(session.meCalls).toBe(0);
  });
});
