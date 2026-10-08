// /invite#<token> (05 §15.1, 03 §7.9) against MSW: the check, the form, every error code of invite/check (#6) and
// register (#5), and the signed-in view.
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import type { InviteInfo } from '../protocol/api.gen';
import { apiError, meFixture } from '../test/msw';
import {
  COMMON_ERRORS,
  fillAccount,
  mockSession,
  networkError,
  noContent,
  onPost,
  NOT_THE_ANSWER,
  PASSWORD_CODES,
  renderPage,
  ROOM_HEADING,
  USERNAME_CODES,
  type ErrorRow,
  type SessionState,
} from './testing/harness';

const TOKEN = 'EXAMPLEinviteTOKEN0123456789abcd';
const INVITE: InviteInfo = {
  serverName: "Alex's server",
  invitedBy: 'Alex',
  expiresAt: '2026-10-15T12:00:00.000Z',
  usesLeft: 9,
};
const FORM_HEADING = "Alex invited you to Alex's server";
const DEAD_HEADING = "This invite link doesn't work";

function checkWorks(info: InviteInfo = INVITE): unknown[] {
  return onPost('/api/v1/auth/invite/check', () => HttpResponse.json(info));
}

/** POST /api/v1/auth/register that creates the account and starts the session. */
function registerWorks(session: SessionState): unknown[] {
  return onPost('/api/v1/auth/register', () => {
    session.me = meFixture();
    return HttpResponse.json(
      { status: 'active', user: { id: 'b8f2n4r6t0vz', username: 'alex', role: 'user' } },
      { status: 201 },
    );
  });
}

function renderInvite() {
  return renderPage({ path: '/invite', token: ['invite', TOKEN] });
}

async function submitAccount(username?: string, password?: string) {
  const user = userEvent.setup();
  await screen.findByRole('heading', { name: FORM_HEADING });
  await fillAccount(user, username, password);
  await user.click(screen.getByRole('button', { name: 'Create account' }));
  return user;
}

/** The codes that say the link can't be used, on the check and on the submit alike. */
const DEAD_LINKS: readonly ErrorRow[] = [
  ['invite_invalid', 404, { code: 'invite_invalid' }, "This invite link doesn't work. Ask for a new one."],
  ['invite_expired', 410, { code: 'invite_expired' }, 'This invite link has expired. Ask for a new one.'],
  ['invite_used_up', 410, { code: 'invite_used_up' }, 'This invite link has been used up. Ask for a new one.'],
  ['invite_revoked', 410, { code: 'invite_revoked' }, 'This invite link was revoked. Ask for a new one.'],
  ['registration_closed', 403, { code: 'registration_closed' }, "This server isn't taking new accounts."],
];

describe('InvitePage', () => {
  it('checks the token, says who invited to which server, registers and lands in the room', async () => {
    const session = mockSession();
    const checked = checkWorks();
    const registered = registerWorks(session);
    const { router, platform } = renderInvite();

    expect(await screen.findByRole('heading', { name: FORM_HEADING })).toBeInTheDocument();
    expect(checked).toEqual([{ token: TOKEN }]);
    // The rules of /info's accountRules, as hints.
    expect(screen.getByLabelText('Username')).toHaveAccessibleDescription(/^2 to 32 characters/);
    expect(screen.getByLabelText('Password')).toHaveAccessibleDescription(/^At least 8 characters/);
    expect(screen.getByLabelText('Password')).toHaveAttribute('autocomplete', 'new-password');

    await submitAccount('太郎', 'a long enough passphrase');
    expect(await screen.findByRole('heading', { name: ROOM_HEADING })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/');
    expect(registered).toEqual([{ inviteToken: TOKEN, username: '太郎', password: 'a long enough passphrase' }]);
    // The token is used up: it leaves the tab's storage (05 §20).
    expect(platform.storage.session.get('isshoni.invite')).toBeNull();
    // The check ran once: the sign-in didn't trigger it again.
    expect(checked).toHaveLength(1);
  });

  it('has no inviter to name when the invite came from the CLI', async () => {
    mockSession();
    checkWorks({ serverName: "Alex's server", expiresAt: INVITE.expiresAt, usesLeft: 1 });
    renderInvite();
    expect(await screen.findByRole('heading', { name: "You're invited to Alex's server" })).toBeInTheDocument();
  });

  it('opened without a token: says the link is incomplete and asks nothing of the server', async () => {
    const session = mockSession();
    const checked = checkWorks();
    renderPage({ path: '/invite' });
    expect(await screen.findByRole('heading', { name: 'This invite link is incomplete' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Log in' })).toHaveAttribute('href', '/login');
    expect(checked).toEqual([]);
    expect(session.meCalls).toBeLessThanOrEqual(1);
  });

  describe('error codes of POST /api/v1/auth/invite/check', () => {
    it.each(DEAD_LINKS)('%s: the reason the link does not work', async (_name, status, error, shown) => {
      mockSession();
      onPost('/api/v1/auth/invite/check', () => apiError(status, error));
      renderInvite();
      expect(await screen.findByRole('heading', { name: DEAD_HEADING })).toBeInTheDocument();
      expect(screen.getByText(shown)).toBeInTheDocument();
      expect(screen.queryByLabelText('Username')).not.toBeInTheDocument();
      expect(screen.getByRole('link', { name: 'Log in' })).toHaveAttribute('href', '/login');
    });

    it('rate_limited: says so and offers to try again, which then works', async () => {
      mockSession();
      let limited = true;
      const checked = onPost('/api/v1/auth/invite/check', () =>
        limited ? apiError(429, { code: 'rate_limited', retryAfter: 30 }) : HttpResponse.json(INVITE),
      );
      renderInvite();
      expect(await screen.findByRole('alert')).toHaveTextContent('Too many attempts. Try again in 30 seconds.');
      // No silent retries: each one would spend the budget (03 §7.3).
      expect(checked).toHaveLength(1);
      limited = false;
      await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
      expect(await screen.findByRole('heading', { name: FORM_HEADING })).toBeInTheDocument();
      expect(checked).toHaveLength(2);
    });

    it.each([
      ['offline', networkError, "You're offline. Check your connection."],
      [
        'internal',
        () => apiError(500, { code: 'internal', requestId: 'req-1' }),
        'Something went wrong on the server. Reference: req-1',
      ],
      ['server_busy', () => apiError(503, { code: 'server_busy' }), 'The server is busy. Try again in a moment.'],
    ])('%s: the check could not finish, which says nothing about the link', async (_name, respond, shown) => {
      mockSession();
      onPost('/api/v1/auth/invite/check', respond);
      renderInvite();
      expect(await screen.findByRole('alert')).toHaveTextContent(shown);
      expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
      expect(screen.queryByRole('heading', { name: DEAD_HEADING })).not.toBeInTheDocument();
    });

    it.each(NOT_THE_ANSWER)('a 200 that is not the answer (%s): the check could not finish', async (_name, respond) => {
      mockSession();
      let inTheWay = true;
      const checked = onPost('/api/v1/auth/invite/check', () => (inTheWay ? respond() : HttpResponse.json(INVITE)));
      renderInvite();
      expect(await screen.findByRole('alert')).toHaveTextContent('Something went wrong (unknown).');
      expect(screen.queryByRole('heading', { name: DEAD_HEADING })).not.toBeInTheDocument();
      inTheWay = false;
      await userEvent.click(screen.getByRole('button', { name: 'Try again' }));
      expect(await screen.findByRole('heading', { name: FORM_HEADING })).toBeInTheDocument();
      expect(checked).toHaveLength(2);
    });
  });

  describe('error codes of POST /api/v1/auth/register', () => {
    it.each(DEAD_LINKS)('%s: the invite stopped working before the submit', async (_name, status, error, shown) => {
      mockSession();
      checkWorks();
      onPost('/api/v1/auth/register', () => apiError(status, error));
      renderInvite();
      await submitAccount();
      const heading = await screen.findByRole('heading', { name: DEAD_HEADING });
      expect(screen.getByText(shown)).toBeInTheDocument();
      expect(screen.queryByLabelText('Username')).not.toBeInTheDocument();
      // The button that had focus went with the form: focus moves to the new heading, so the change is heard.
      expect(heading).toBe(screen.getByRole('heading', { level: 1 }));
      await waitFor(() => {
        expect(heading).toHaveFocus();
      });
    });

    it('username_taken: under the username, which takes focus', async () => {
      mockSession();
      checkWorks();
      onPost('/api/v1/auth/register', () => apiError(409, { code: 'username_taken' }));
      renderInvite();
      await submitAccount();
      const username = screen.getByLabelText('Username');
      await waitFor(() => {
        expect(username).toBeInvalid();
      });
      expect(username).toHaveAccessibleDescription(/That username is taken\.$/);
      expect(username).toHaveFocus();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    });

    it.each(USERNAME_CODES)('validation_failed, username %s', async (code, shown) => {
      mockSession();
      checkWorks();
      onPost('/api/v1/auth/register', () => apiError(422, { code: 'validation_failed', fields: { username: code } }));
      renderInvite();
      await submitAccount();
      const username = screen.getByLabelText('Username');
      await waitFor(() => {
        expect(username).toBeInvalid();
      });
      expect(username).toHaveAccessibleDescription(new RegExp(`${escapeRegExp(shown)}$`));
      expect(screen.getByLabelText('Password')).toBeValid();
    });

    it.each(PASSWORD_CODES)('validation_failed, password %s', async (code, shown) => {
      mockSession();
      checkWorks();
      onPost('/api/v1/auth/register', () => apiError(422, { code: 'validation_failed', fields: { password: code } }));
      renderInvite();
      await submitAccount();
      const password = screen.getByLabelText('Password');
      await waitFor(() => {
        expect(password).toBeInvalid();
      });
      expect(password).toHaveAccessibleDescription(new RegExp(`${escapeRegExp(shown)}$`));
      expect(password).toHaveFocus();
    });

    it('invite_required: the server did not see the invite', async () => {
      mockSession();
      checkWorks();
      onPost('/api/v1/auth/register', () => apiError(403, { code: 'invite_required' }));
      renderInvite();
      await submitAccount();
      expect(await screen.findByRole('alert')).toHaveTextContent('You need an invite link to join this server.');
    });

    it('limit_reached: names the limit', async () => {
      mockSession();
      checkWorks();
      onPost('/api/v1/auth/register', () =>
        apiError(409, { code: 'limit_reached', params: { limit: 'pending_signups' } }),
      );
      renderInvite();
      await submitAccount();
      expect(await screen.findByRole('alert')).toHaveTextContent(
        'Too many sign-ups are waiting for approval. Try again later.',
      );
    });

    it('rate_limited and server_busy with a wait: shown, with the button off', async () => {
      mockSession();
      checkWorks();
      let code = 'rate_limited';
      onPost('/api/v1/auth/register', () => apiError(code === 'rate_limited' ? 429 : 503, { code, retryAfter: 1 }));
      renderInvite();
      const user = await submitAccount();
      expect(await screen.findByRole('alert')).toHaveTextContent('Too many attempts. Try again in 1 second.');
      const submit = screen.getByRole('button', { name: 'Create account' });
      expect(submit).toBeDisabled();
      // When the wait is over the message goes and the form can be sent again.
      await waitFor(
        () => {
          expect(submit).toBeEnabled();
        },
        { timeout: 3000 },
      );
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      code = 'server_busy';
      await user.click(submit);
      expect(await screen.findByRole('alert')).toHaveTextContent('The server is busy. Try again in 1 second.');
    });

    it.each(COMMON_ERRORS)('%s', async (_name, status, error, shown) => {
      mockSession();
      checkWorks();
      onPost('/api/v1/auth/register', () => apiError(status, error));
      renderInvite();
      await submitAccount();
      expect(await screen.findByRole('alert')).toHaveTextContent(shown);
      expect(screen.getByRole('button', { name: 'Create account' })).toBeEnabled();
    });
  });

  describe('before the request', () => {
    it.each([
      ['', 'correct horse battery', 'Username', 'Enter a username.'],
      ['a', 'correct horse battery', 'Username', "That's too short for a username."],
      ['x'.repeat(33), 'correct horse battery', 'Username', "That's too long for a username."],
      ['alex', '', 'Password', 'Enter a password.'],
      ['alex', 'short', 'Password', "That's too short for a password."],
      ['alex', 'p'.repeat(129), 'Password', "That's too long for a password."],
    ])('checks the account rules of /info: %j / %j', async (username, password, label, shown) => {
      mockSession();
      checkWorks();
      const registered = onPost('/api/v1/auth/register', () => apiError(500, { code: 'internal' }));
      renderInvite();
      await submitAccount(username, password);
      const field = screen.getByLabelText(label);
      expect(field).toBeInvalid();
      expect(field).toHaveAccessibleDescription(new RegExp(`${escapeRegExp(shown)}$`));
      expect(field).toHaveFocus();
      expect(registered).toEqual([]);
    });

    it('counts characters, not UTF-16 units: a two-character name in another script passes', async () => {
      const session = mockSession();
      checkWorks();
      const registered = registerWorks(session);
      renderInvite();
      await submitAccount('太郎');
      expect(await screen.findByRole('heading', { name: ROOM_HEADING })).toBeInTheDocument();
      expect(registered).toHaveLength(1);
    });
  });

  describe('a signed-in user', () => {
    it('sees who they are, and can go to the app', async () => {
      mockSession(meFixture());
      const checked = checkWorks();
      renderInvite();
      expect(await screen.findByRole('heading', { name: "You're already in" })).toBeInTheDocument();
      expect(screen.getByText("You're signed in as alex.")).toBeInTheDocument();
      expect(screen.getByRole('link', { name: 'Go to isshoni' })).toHaveAttribute('href', '/');
      expect(screen.queryByLabelText('Username')).not.toBeInTheDocument();
      // No check until it is needed.
      expect(checked).toEqual([]);
    });

    it('"Sign out and create another account" logs out and shows the form', async () => {
      const session = mockSession(meFixture());
      const checked = checkWorks();
      const loggedOut = onPost('/api/v1/auth/logout', () => {
        session.me = null;
        return noContent();
      });
      const { router, platform } = renderInvite();
      await userEvent.click(await screen.findByRole('button', { name: 'Sign out and create another account' }));
      expect(await screen.findByRole('heading', { name: FORM_HEADING })).toBeInTheDocument();
      expect(loggedOut).toEqual([{}]);
      expect(checked).toEqual([{ token: TOKEN }]);
      // A public page stays where it is, and the invite is still there to use.
      expect(router.state.location.pathname).toBe('/invite');
      expect(platform.storage.session.get('isshoni.invite')).toBe(TOKEN);
    });

    it('stays signed in, with a toast, when the logout cannot reach the server', async () => {
      mockSession(meFixture());
      checkWorks();
      onPost('/api/v1/auth/logout', networkError);
      const { services } = renderInvite();
      await userEvent.click(await screen.findByRole('button', { name: 'Sign out and create another account' }));
      await waitFor(() => {
        expect(services.ui.getState().toasts.map((toast) => toast.message)).toEqual([
          "Couldn't sign out. You're offline. Check your connection.",
        ]);
      });
      expect(screen.getByText("You're signed in as alex.")).toBeInTheDocument();
    });
  });
});

function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}
