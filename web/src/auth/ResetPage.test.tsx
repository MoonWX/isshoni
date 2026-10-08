// /reset#<token> (05 §15.1, 03 §7.10) against MSW: the check, the new password, and every error code of reset/check
// (#9) and reset/complete (#10).
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import { apiError, meFixture } from '../test/msw';
import {
  COMMON_ERRORS,
  mockSession,
  networkError,
  NOT_THE_ANSWER,
  onPost,
  PASSWORD_CODES,
  renderPage,
  ROOM_HEADING,
  typeInto,
  type ErrorRow,
  type SessionState,
} from './testing/harness';

const TOKEN = 'EXAMPLEresetTOKEN0123456789abcdef';
const HEADING = 'Choose a new password';
const DEAD_HEADING = "This reset link doesn't work";
const DEAD_TEXT = "This reset link doesn't work anymore. Ask an admin for a new one.";

function checkWorks(): unknown[] {
  return onPost('/api/v1/auth/reset/check', () => HttpResponse.json({ username: 'Sam' }));
}

function completeWorks(session: SessionState): unknown[] {
  return onPost('/api/v1/auth/reset/complete', () => {
    session.me = meFixture();
    return HttpResponse.json({ user: { id: 'k3m9p2qxw7ht', username: 'Sam', role: 'user' } });
  });
}

function renderReset() {
  return renderPage({ path: '/reset', token: ['reset', TOKEN] });
}

async function submitPassword(password = 'a brand new passphrase') {
  const user = userEvent.setup();
  await screen.findByLabelText('New password');
  await typeInto(user, 'New password', password);
  await user.click(screen.getByRole('button', { name: 'Save and log in' }));
  return user;
}

describe('ResetPage', () => {
  it('checks the token, shows whose password it is, saves the new one and logs in', async () => {
    const session = mockSession();
    const checked = checkWorks();
    const completed = completeWorks(session);
    const { router, platform } = renderReset();

    expect(await screen.findByRole('heading', { name: HEADING })).toBeInTheDocument();
    const username = await screen.findByLabelText('Username');
    expect(username).toHaveValue('Sam');
    expect(username).toHaveAttribute('readonly');
    expect(checked).toEqual([{ token: TOKEN }]);
    expect(screen.getByLabelText('New password')).toHaveAttribute('autocomplete', 'new-password');
    expect(screen.getByLabelText('New password')).toHaveAccessibleDescription(/^At least 8 characters/);

    await submitPassword();
    expect(await screen.findByRole('heading', { name: ROOM_HEADING })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/');
    expect(completed).toEqual([{ token: TOKEN, password: 'a brand new passphrase' }]);
    expect(platform.storage.session.get('isshoni.reset')).toBeNull();
    expect(checked).toHaveLength(1);
  });

  it('opened without a token: says the link is incomplete and asks nothing of the server', async () => {
    mockSession();
    const checked = checkWorks();
    renderPage({ path: '/reset' });
    expect(await screen.findByRole('heading', { name: 'This reset link is incomplete' })).toBeInTheDocument();
    expect(checked).toEqual([]);
  });

  describe('error codes of POST /api/v1/auth/reset/check', () => {
    it('reset_token_invalid: unknown, used or expired', async () => {
      mockSession();
      onPost('/api/v1/auth/reset/check', () => apiError(404, { code: 'reset_token_invalid' }));
      renderReset();
      expect(await screen.findByRole('heading', { name: DEAD_HEADING })).toBeInTheDocument();
      expect(screen.getByText(DEAD_TEXT)).toBeInTheDocument();
      expect(screen.queryByLabelText('New password')).not.toBeInTheDocument();
      expect(screen.getByRole('link', { name: 'Log in' })).toHaveAttribute('href', '/login');
    });

    it.each([
      ['offline', networkError, "You're offline. Check your connection."],
      [
        'rate_limited',
        () => apiError(429, { code: 'rate_limited', retryAfter: 9 }),
        'Too many attempts. Try again in 9 seconds.',
      ],
      [
        'internal',
        () => apiError(500, { code: 'internal', requestId: 'req-2' }),
        'Something went wrong on the server. Reference: req-2',
      ],
    ])('%s: the check could not finish; "Try again" asks once more', async (_name, respond, shown) => {
      mockSession();
      let failing = true;
      const checked = onPost('/api/v1/auth/reset/check', () =>
        failing ? respond() : HttpResponse.json({ username: 'Sam' }),
      );
      renderReset();
      expect(await screen.findByRole('alert')).toHaveTextContent(shown);
      expect(screen.queryByRole('heading', { name: DEAD_HEADING })).not.toBeInTheDocument();
      failing = false;
      await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
      expect(await screen.findByLabelText('New password')).toBeInTheDocument();
      expect(checked).toHaveLength(2);
    });

    it.each(NOT_THE_ANSWER)('a 200 that is not the answer (%s): the check could not finish', async (_name, respond) => {
      mockSession();
      let inTheWay = true;
      const checked = onPost('/api/v1/auth/reset/check', () =>
        inTheWay ? respond() : HttpResponse.json({ username: 'Sam' }),
      );
      renderReset();
      expect(await screen.findByRole('alert')).toHaveTextContent('Something went wrong (unknown).');
      expect(screen.queryByRole('heading', { name: DEAD_HEADING })).not.toBeInTheDocument();
      inTheWay = false;
      await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
      expect(await screen.findByLabelText('Username')).toHaveValue('Sam');
      expect(checked).toHaveLength(2);
    });
  });

  describe('error codes of POST /api/v1/auth/reset/complete', () => {
    it('reset_token_invalid: the link was used or expired in the meantime', async () => {
      mockSession();
      checkWorks();
      onPost('/api/v1/auth/reset/complete', () => apiError(404, { code: 'reset_token_invalid' }));
      renderReset();
      await submitPassword();
      const heading = await screen.findByRole('heading', { name: DEAD_HEADING });
      expect(screen.getByText(DEAD_TEXT)).toBeInTheDocument();
      // The button that had focus went with the form: focus moves to the new heading, so the change is heard.
      expect(heading).toBe(screen.getByRole('heading', { level: 1 }));
      await waitFor(() => {
        expect(heading).toHaveFocus();
      });
    });

    it.each(PASSWORD_CODES)('validation_failed, password %s', async (code, shown) => {
      mockSession();
      checkWorks();
      onPost('/api/v1/auth/reset/complete', () =>
        apiError(422, { code: 'validation_failed', fields: { password: code } }),
      );
      renderReset();
      await submitPassword();
      const password = screen.getByLabelText('New password');
      await waitFor(() => {
        expect(password).toBeInvalid();
      });
      expect(screen.getByText(shown)).toBeInTheDocument();
      expect(password).toHaveFocus();
    });

    it.each<ErrorRow>([
      ['rate_limited', 429, { code: 'rate_limited', retryAfter: 42 }, 'Too many attempts. Try again in 42 seconds.'],
      ['server_busy', 503, { code: 'server_busy', retryAfter: 5 }, 'The server is busy. Try again in 5 seconds.'],
      ...COMMON_ERRORS,
    ])('%s', async (_name, status, error, shown) => {
      mockSession();
      checkWorks();
      onPost('/api/v1/auth/reset/complete', () => apiError(status, error));
      const { platform } = renderReset();
      await submitPassword();
      expect(await screen.findByRole('alert')).toHaveTextContent(shown);
      // The link isn't used up: the token stays for the next try.
      expect(platform.storage.session.get('isshoni.reset')).toBe(TOKEN);
    });
  });

  it.each([
    ['', 'Enter a password.'],
    ['short', "That's too short for a password."],
    ['p'.repeat(129), "That's too long for a password."],
  ])('checks the password rules of /info before it sends anything: %j', async (password, shown) => {
    mockSession();
    checkWorks();
    const completed = onPost('/api/v1/auth/reset/complete', () => apiError(500, { code: 'internal' }));
    renderReset();
    await submitPassword(password);
    expect(screen.getByText(shown)).toBeInTheDocument();
    expect(screen.getByLabelText('New password')).toHaveFocus();
    expect(completed).toEqual([]);
  });
});
