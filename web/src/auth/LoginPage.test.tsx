// /login (05 §15.1) against MSW: the way in, every error code the endpoint answers (03 §12.3 #2), and what the page
// says around the form.
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { apiError, meFixture } from '../test/msw';
import { loginNoticeCode, loginState } from './loginNotice';
import { afterLogin } from './LoginPage';
import {
  COMMON_ERRORS,
  mockSession,
  networkError,
  onPost,
  renderPage,
  ROOM_HEADING,
  typeInto,
  type SessionState,
} from './testing/harness';

const HEADING = 'Log in to Test server';

/** POST /api/v1/auth/login that accepts alex and starts the session. */
function loginWorks(session: SessionState): unknown[] {
  return onPost('/api/v1/auth/login', () => {
    session.me = meFixture();
    return HttpResponse.json({ user: { id: 'k3m9p2qxw7ht', username: 'alex', role: 'user' } });
  });
}

async function submitLogin(username = 'Alex', password = 'correct horse battery') {
  const user = userEvent.setup();
  await screen.findByRole('heading', { name: HEADING });
  await typeInto(user, 'Username', username);
  await typeInto(user, 'Password', password);
  await user.click(screen.getByRole('button', { name: 'Log in' }));
  return user;
}

afterEach(() => {
  vi.useRealTimers();
});

describe('LoginPage', () => {
  it('logs in and goes to the default room', async () => {
    const session = mockSession();
    const posted = loginWorks(session);
    const { router } = renderPage({ path: '/login' });
    await submitLogin();
    expect(await screen.findByRole('heading', { name: ROOM_HEADING })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/');
    // The username goes as typed (the server normalizes it, 03 §7.1), in a JSON body.
    expect(posted).toEqual([{ username: 'Alex', password: 'correct horse battery' }]);
  });

  it('goes on to ?next= after the login', async () => {
    const session = mockSession();
    loginWorks(session);
    const { router } = renderPage({ path: `/login?next=${encodeURIComponent('/account?tab=pw')}` });
    await submitLogin();
    expect(await screen.findByRole('heading', { name: 'account' })).toBeInTheDocument();
    expect(router.state.location.pathname + router.state.location.search).toBe('/account?tab=pw');
  });

  it.each(['//evil.example/x', 'https://evil.example/', '/\\evil.example'])(
    'ignores a next that leaves the site: %s',
    async (next) => {
      const session = mockSession();
      loginWorks(session);
      const { router } = renderPage({ path: `/login?next=${encodeURIComponent(next)}` });
      await submitLogin();
      expect(await screen.findByRole('heading', { name: ROOM_HEADING })).toBeInTheDocument();
      expect(router.state.location.pathname).toBe('/');
    },
  );

  it('sends someone who is already signed in straight on', async () => {
    mockSession(meFixture());
    const { router } = renderPage({ path: '/login?next=%2Faccount' });
    expect(await screen.findByRole('heading', { name: 'account' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/account');
  });

  it('asks the server about a user it only has cached before it sends them on', async () => {
    const session = mockSession(meFixture());
    const { router } = renderPage({ path: '/account' });
    await screen.findByRole('heading', { name: 'account' });
    const asked = session.meCalls;
    await act(() => router.navigate('/login?next=%2Faccount%3Ftab%3Dpw'));
    await waitFor(() => {
      expect(router.state.location.pathname + router.state.location.search).toBe('/account?tab=pw');
    });
    expect(await screen.findByRole('heading', { name: 'account' })).toBeInTheDocument();
    // The cache was fresh, and the page asked all the same.
    expect(session.meCalls).toBe(asked + 1);
  });

  it('shows the form when the server no longer knows the cached user, instead of sending them back', async () => {
    const session = mockSession(meFixture());
    const { router } = renderPage({ path: '/account' });
    await screen.findByRole('heading', { name: 'account' });
    // The session ended on the server (an admin revoked it); this tab's cache hasn't heard.
    session.me = null;
    const asked = session.meCalls;
    await act(() => router.navigate('/login?next=%2Faccount'));
    expect(await screen.findByRole('heading', { name: HEADING })).toBeInTheDocument();
    expect(router.state.location.pathname + router.state.location.search).toBe('/login?next=%2Faccount');
    expect(session.meCalls).toBe(asked + 1);
  });

  it.each(['/login', '/login?next=%2Flogin', '/login/', '/login#x'])(
    'a next that points back at the login page (%s) goes to the room instead',
    async (next) => {
      expect(afterLogin(next)).toBe('/');
      // Signed in already: no loop.
      mockSession(meFixture());
      const { router } = renderPage({ path: `/login?next=${encodeURIComponent(next)}` });
      expect(await screen.findByRole('heading', { name: ROOM_HEADING })).toBeInTheDocument();
      expect(router.state.location.pathname).toBe('/');
    },
  );

  it('afterLogin keeps other paths, and only same-origin ones', () => {
    expect(afterLogin('/r/lounge?focus=s_1')).toBe('/r/lounge?focus=s_1');
    expect(afterLogin('/loginhelp')).toBe('/loginhelp');
    expect(afterLogin(null)).toBe('/');
    expect(afterLogin('//evil.example/login')).toBe('/');
  });

  it('asks for both fields before it sends anything', async () => {
    mockSession();
    const posted = onPost('/api/v1/auth/login', () => apiError(401, { code: 'invalid_credentials' }));
    renderPage({ path: '/login' });
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Log in' }));
    const username = screen.getByLabelText('Username');
    expect(username).toHaveAccessibleDescription('Enter a username.');
    expect(username).toBeInvalid();
    expect(screen.getByLabelText('Password')).toHaveAccessibleDescription('Enter a password.');
    // Focus moves to the first field that needs fixing (05 §16.6).
    expect(username).toHaveFocus();
    expect(posted).toEqual([]);
  });

  it('shows and hides the password', async () => {
    mockSession();
    renderPage({ path: '/login' });
    const user = userEvent.setup();
    const password = await screen.findByLabelText('Password');
    expect(password).toHaveAttribute('type', 'password');
    expect(password).toHaveAttribute('autocomplete', 'current-password');
    const toggle = screen.getByRole('button', { name: 'Show password' });
    expect(toggle).toHaveAttribute('aria-pressed', 'false');
    await user.click(toggle);
    expect(password).toHaveAttribute('type', 'text');
    expect(toggle).toHaveAttribute('aria-pressed', 'true');
    await user.click(toggle);
    expect(password).toHaveAttribute('type', 'password');
  });

  describe('error codes of POST /api/v1/auth/login', () => {
    it('invalid_credentials: one message for a wrong username or password, and the form stays filled in', async () => {
      mockSession();
      onPost('/api/v1/auth/login', () => apiError(401, { code: 'invalid_credentials' }));
      const { router } = renderPage({ path: '/login' });
      await submitLogin('alex', 'not my password');
      expect(await screen.findByRole('alert')).toHaveTextContent('Wrong username or password.');
      expect(router.state.location.pathname).toBe('/login');
      expect(screen.getByLabelText('Username')).toHaveValue('alex');
      expect(screen.getByRole('button', { name: 'Log in' })).toBeEnabled();
    });

    it('account_pending: the password was right, the approval is missing → /pending', async () => {
      mockSession();
      onPost('/api/v1/auth/login', () => apiError(403, { code: 'account_pending' }));
      const { router } = renderPage({ path: '/login' });
      await submitLogin();
      expect(await screen.findByRole('heading', { name: 'Request sent' })).toBeInTheDocument();
      expect(router.state.location.pathname).toBe('/pending');
      expect(screen.getByText('An admin will review your request. Try logging in later.')).toBeInTheDocument();
    });

    it('account_disabled: a notice', async () => {
      mockSession();
      onPost('/api/v1/auth/login', () => apiError(403, { code: 'account_disabled' }));
      renderPage({ path: '/login' });
      await submitLogin();
      expect(await screen.findByRole('alert')).toHaveTextContent('Your account was disabled. Ask an admin.');
    });

    it('validation_failed: each field shows its code', async () => {
      mockSession();
      onPost('/api/v1/auth/login', () =>
        apiError(422, { code: 'validation_failed', fields: { username: 'invalid', password: 'too_long' } }),
      );
      renderPage({ path: '/login' });
      await submitLogin();
      const username = screen.getByLabelText('Username');
      await waitFor(() => {
        expect(username).toBeInvalid();
      });
      expect(username).toHaveAccessibleDescription(
        'Use letters and numbers, with single dots, dashes or underscores in between.',
      );
      expect(screen.getByLabelText('Password')).toHaveAccessibleDescription("That's too long for a password.");
      expect(username).toHaveFocus();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    });

    it('validation_failed about a field the form does not have: the general message', async () => {
      mockSession();
      onPost('/api/v1/auth/login', () => apiError(422, { code: 'validation_failed', fields: { device: 'required' } }));
      renderPage({ path: '/login' });
      await submitLogin();
      expect(await screen.findByRole('alert')).toHaveTextContent('Check the highlighted fields.');
    });

    it('rate_limited (429): shows the wait, counts it down and keeps the button off until it is over', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      mockSession();
      const posted = onPost('/api/v1/auth/login', () => apiError(429, { code: 'rate_limited', retryAfter: 42 }));
      renderPage({ path: '/login' });
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime.bind(vi) });
      await screen.findByRole('heading', { name: HEADING });
      await typeInto(user, 'Username', 'alex');
      await typeInto(user, 'Password', 'correct horse battery');
      await user.click(screen.getByRole('button', { name: 'Log in' }));

      const alert = await screen.findByRole('alert');
      // Screen readers hear the wait once; the visible text is the one that counts down.
      expect(within(alert).getAllByText('Too many attempts. Try again in 42 seconds.')).toHaveLength(2);
      const submit = screen.getByRole('button', { name: 'Log in' });
      expect(submit).toBeDisabled();
      await user.click(submit);
      expect(posted).toHaveLength(1);

      await act(() => vi.advanceTimersByTimeAsync(41_000));
      expect(alert).toHaveTextContent('Try again in 1 second.');
      expect(submit).toBeDisabled();

      await act(() => vi.advanceTimersByTimeAsync(1_500));
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      expect(submit).toBeEnabled();
    });

    it('server_busy (503) with a wait: shows it', async () => {
      mockSession();
      onPost('/api/v1/auth/login', () => apiError(503, { code: 'server_busy', retryAfter: 5 }));
      renderPage({ path: '/login' });
      await submitLogin();
      expect(await screen.findByRole('alert')).toHaveTextContent('The server is busy. Try again in 5 seconds.');
      expect(screen.getByRole('button', { name: 'Log in' })).toBeDisabled();
    });

    it('rate_limited with only a Retry-After header: the same wait', async () => {
      mockSession();
      onPost('/api/v1/auth/login', () =>
        HttpResponse.json({ error: { code: 'rate_limited' } }, { status: 429, headers: { 'Retry-After': '1' } }),
      );
      renderPage({ path: '/login' });
      await submitLogin();
      expect(await screen.findByRole('alert')).toHaveTextContent('Too many attempts. Try again in 1 second.');
    });

    it.each(COMMON_ERRORS)('%s', async (_name, status, error, shown) => {
      mockSession();
      onPost('/api/v1/auth/login', () => apiError(status, error));
      renderPage({ path: '/login' });
      await submitLogin();
      expect(await screen.findByRole('alert')).toHaveTextContent(shown);
      expect(screen.getByRole('button', { name: 'Log in' })).toBeEnabled();
    });

    it('a response that is not the error envelope (a proxy page)', async () => {
      mockSession();
      onPost('/api/v1/auth/login', () => new HttpResponse('<html>Bad Gateway</html>', { status: 502 }));
      renderPage({ path: '/login' });
      await submitLogin();
      expect(await screen.findByRole('alert')).toHaveTextContent('Something went wrong (unknown).');
    });

    it('no response at all: offline', async () => {
      mockSession();
      onPost('/api/v1/auth/login', networkError);
      renderPage({ path: '/login' });
      await submitLogin();
      expect(await screen.findByRole('alert')).toHaveTextContent("You're offline. Check your connection.");
    });

    it('a 200 without a session (the browser dropped the cookie): says so instead of looping', async () => {
      const session = mockSession();
      onPost('/api/v1/auth/login', () =>
        HttpResponse.json({ user: { id: 'k3m9p2qxw7ht', username: 'alex', role: 'user' } }),
      );
      const { router } = renderPage({ path: '/login' });
      await submitLogin();
      expect(await screen.findByRole('alert')).toHaveTextContent(/didn't keep you signed in\. Allow cookies/);
      expect(router.state.location.pathname).toBe('/login');
      expect(session.meCalls).toBeGreaterThan(0);
    });
  });

  describe('around the form', () => {
    it('says where passwords and accounts come from (invite mode)', async () => {
      mockSession();
      renderPage({ path: '/login' });
      expect(await screen.findByText('Forgot your password? Ask an admin for a reset link.')).toBeInTheDocument();
      expect(screen.getByText('Need an account? Ask a friend for an invite link.')).toBeInTheDocument();
      expect(screen.queryByRole('link', { name: 'Request one' })).not.toBeInTheDocument();
    });

    it('links to /signup in approval mode', async () => {
      mockSession();
      renderPage({ path: '/login', info: { registration: 'approval' } });
      const link = await screen.findByRole('link', { name: 'Request one' });
      expect(link).toHaveAttribute('href', '/signup');
      expect(link.parentElement).toHaveTextContent('Need an account? Request one');
    });

    it('states the trust model in three lines and links to /about', async () => {
      mockSession();
      renderPage({ path: '/login' });
      const trust = await screen.findByRole('region', { name: 'Before you log in' });
      const lines = within(trust).getAllByRole('listitem');
      expect(lines.map((li) => li.textContent)).toEqual([
        'This server is trusted: whoever runs it could see the streams.',
        'Nothing is recorded, and nobody watches unseen.',
        'No telemetry.',
      ]);
      expect(within(trust).getByRole('link', { name: 'How isshoni handles privacy' })).toHaveAttribute(
        'href',
        '/about',
      );
    });

    it('shows why the user was sent here when the navigation says so', async () => {
      mockSession();
      const { router } = renderPage({ path: '/account' });
      await screen.findByRole('heading', { name: HEADING });
      expect(screen.queryByRole('status')).not.toBeInTheDocument();
      await act(() => router.navigate('/login', { state: loginState('session_revoked') }));
      expect(await screen.findByRole('status')).toHaveTextContent('You were signed out.');
    });

    it('shows it when the session ended under a tab that still has the user cached', async () => {
      // 05 §6.3: a `session`-scope error ends the session, and the connection wiring navigates here with the notice.
      // Nothing has told this tab's ['me'] yet.
      const session = mockSession(meFixture());
      const { router } = renderPage({ path: '/account' });
      await screen.findByRole('heading', { name: 'account' });
      session.me = null;
      await act(() => router.navigate('/login?next=%2Faccount', { state: loginState('session_revoked') }));
      const heading = await screen.findByRole('heading', { name: HEADING });
      expect(screen.getByRole('status')).toHaveTextContent('You were signed out.');
      expect(router.state.location.pathname).toBe('/login');
      // Focus is on the page heading, as after any navigation (05 §16.6), although the form came after the answer.
      await waitFor(() => {
        expect(heading).toHaveFocus();
      });
    });

    it('reads only a notice code from the location state; whatever else history holds is ignored', async () => {
      expect(loginNoticeCode(loginState('session_revoked'))).toBe('session_revoked');
      for (const state of [null, undefined, 'session_revoked', 3, {}, { notice: 3 }, { notice: '' }]) {
        expect(loginNoticeCode(state)).toBeNull();
      }
      // A code this build has no text for still says something.
      mockSession();
      const { router } = renderPage({ path: '/login' });
      await screen.findByRole('heading', { name: HEADING });
      await act(() => router.navigate('/login', { state: loginState('from_a_newer_server') }));
      expect(await screen.findByRole('status')).toHaveTextContent('Something went wrong (from_a_newer_server).');
    });
  });
});
