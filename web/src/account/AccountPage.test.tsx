// /account (05 §15.2) against MSW: who is signed in, Sign out, the password change (03 §12.3 #12) and deleting the
// account (#13), each with the error codes its endpoint answers.
import { screen, waitFor, within } from '@testing-library/react';
import userEvent, { type UserEvent } from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { CHANNEL_NAME } from '../app/session';
import {
  COMMON_ERRORS,
  networkError,
  noContent,
  onPost,
  typeInto,
  type ErrorRow,
  type FieldRow,
} from '../auth/testing/harness';
import { queryKeys } from '../protocol/queryKeys';
import { apiError, meFixture } from '../test/msw';
import { LOGIN_HEADING, renderAccount } from './testing/harness';

const HEADING = 'Your account';
const CHANGED = 'Password changed. Your other devices were signed out.';
const WRONG_PASSWORD = "Your current password isn't right.";
const DELETE_DIALOG = 'Delete your account?';

/** The errors of COMMON_ERRORS that a form shows as they are (the two with a wait have their own tests). */
const SHOWN_ABOVE: readonly ErrorRow[] = COMMON_ERRORS;

/** 03 §7.2's password codes, as the server names them for `newPassword`. */
const NEW_PASSWORD_CODES: readonly FieldRow[] = [
  ['required', 'Enter a new password.'],
  ['too_short', "That's too short for a password."],
  ['too_long', "That's too long for a password."],
  ['invalid', "That password has characters that can't be used."],
  ['too_common', "That password is too common. Pick one that's harder to guess."],
  ['same_as_username', "Your password can't be your username."],
  // A field code without a text of its own falls back to the general one.
  ['not_allowed', 'Check the highlighted fields.'],
];

async function ready(): Promise<UserEvent> {
  await screen.findByRole('heading', { name: HEADING });
  return userEvent.setup();
}

describe('AccountPage', () => {
  it('shows who is signed in, with the links between the account pages and the way back', async () => {
    renderAccount('/account');
    expect(await screen.findByRole('heading', { level: 1, name: HEADING })).toBeInTheDocument();

    const profile = screen.getByRole('region', { name: 'Profile' });
    expect(within(profile).getByText('Username').nextElementSibling).toHaveTextContent('alex');
    expect(within(profile).getByText('Role').nextElementSibling).toHaveTextContent('Member');

    const nav = screen.getByRole('navigation', { name: 'Account' });
    const links = within(nav).getAllByRole('link');
    expect(links.map((a) => [a.textContent, a.getAttribute('href')])).toEqual([
      ['Account', '/account'],
      ['Devices', '/account/devices'],
      ['Notifications', '/account/notifications'],
    ]);
    expect(within(nav).getByRole('link', { name: 'Account' })).toHaveAttribute('aria-current', 'page');
    expect(within(nav).getByRole('link', { name: 'Devices' })).not.toHaveAttribute('aria-current');
    expect(screen.getByRole('link', { name: 'Back to isshoni' })).toHaveAttribute('href', '/');
    // Everything sits in the page's main landmark.
    expect(screen.getByRole('main')).toContainElement(nav);
  });

  it('says Admin for an admin', async () => {
    renderAccount('/account', { me: meFixture({ admin: true }) });
    await screen.findByRole('heading', { name: HEADING });
    const profile = screen.getByRole('region', { name: 'Profile' });
    expect(within(profile).getByText('Username').nextElementSibling).toHaveTextContent('admin');
    expect(within(profile).getByText('Role').nextElementSibling).toHaveTextContent('Admin');
  });

  it('Sign out runs the logout flow and ends on the login page', async () => {
    const { session, router, services } = renderAccount('/account');
    const loggedOut = onPost('/api/v1/auth/logout', () => {
      session.me = null;
      return noContent();
    });
    const user = await ready();
    await user.click(screen.getByRole('button', { name: 'Sign out' }));

    expect(await screen.findByRole('heading', { name: LOGIN_HEADING })).toBeInTheDocument();
    expect(loggedOut).toEqual([{}]);
    expect(services.queryClient.getQueryData(queryKeys.me)).toBeNull();
    expect(router.state.location.pathname + router.state.location.search).toBe('/login?next=%2Faccount');
  });

  it('stays signed in, with a message, when the server cannot be reached for the sign-out', async () => {
    const { services } = renderAccount('/account');
    onPost('/api/v1/auth/logout', networkError);
    const user = await ready();
    await user.click(screen.getByRole('button', { name: 'Sign out' }));
    await waitFor(() => {
      expect(services.ui.getState().toasts.map((toast) => toast.message)).toEqual([
        "Couldn't sign out. You're offline. Check your connection.",
      ]);
    });
    expect(screen.getByRole('heading', { name: HEADING })).toBeInTheDocument();
    expect(services.queryClient.getQueryData(queryKeys.me)).not.toBeNull();
  });
});

describe('AccountPage: change password', () => {
  async function change(user: UserEvent, current: string, next: string): Promise<void> {
    await typeInto(user, 'Current password', current);
    await typeInto(user, 'New password', next);
    await user.click(screen.getByRole('button', { name: 'Change password' }));
  }

  it('says up front what it does to the other devices, and what a password needs', async () => {
    renderAccount('/account');
    await ready();
    const section = screen.getByRole('region', { name: 'Change password' });
    expect(
      within(section).getByText('Changing your password signs you out on your other devices.'),
    ).toBeInTheDocument();
    expect(screen.getByLabelText('Current password')).toHaveAttribute('autocomplete', 'current-password');
    expect(screen.getByLabelText('New password')).toHaveAttribute('autocomplete', 'new-password');
    // The rule of /info's accountRules, as a hint.
    expect(screen.getByLabelText('New password')).toHaveAccessibleDescription(/^At least 8 characters/);
    expect(screen.getByLabelText('Current password')).toHaveAttribute('type', 'password');
  });

  it('changes it: both passwords go to the server, the page confirms and the form starts over', async () => {
    const posted = onPost('/api/v1/me/password', () => HttpResponse.json({}));
    const { services, router } = renderAccount('/account');
    // What the devices page had loaded is out of date afterwards.
    services.queryClient.setQueryData(queryKeys.meSessions, { sessions: [] });
    services.queryClient.setQueryData(queryKeys.meDevices, { devices: [] });
    const user = await ready();
    expect(screen.queryByText(CHANGED)).not.toBeInTheDocument();

    await change(user, 'correct horse battery', 'a much better passphrase');

    const done = await screen.findByText(CHANGED);
    expect(posted).toEqual([{ currentPassword: 'correct horse battery', newPassword: 'a much better passphrase' }]);
    // The button that had focus is gone with the old form: the confirmation takes it.
    await waitFor(() => {
      expect(done.closest('p')).toHaveFocus();
    });
    expect(screen.getByLabelText('Current password')).toHaveValue('');
    expect(screen.getByLabelText('New password')).toHaveValue('');
    expect(screen.getByRole('button', { name: 'Change password' })).not.toHaveAttribute('aria-busy');
    expect(services.queryClient.getQueryState(queryKeys.meSessions)?.isInvalidated).toBe(true);
    expect(services.queryClient.getQueryState(queryKeys.meDevices)?.isInvalidated).toBe(true);
    // This browser stays signed in, on the same page.
    expect(router.state.location.pathname).toBe('/account');
    expect(services.queryClient.getQueryData(queryKeys.me)).not.toBeNull();

    // And it works again.
    await change(user, 'a much better passphrase', 'an even better passphrase');
    await waitFor(() => {
      expect(posted).toHaveLength(2);
    });
    expect(posted[1]).toEqual({
      currentPassword: 'a much better passphrase',
      newPassword: 'an even better passphrase',
    });
    expect(await screen.findByText(CHANGED)).toBeInTheDocument();
  });

  it('wrong_password (403): says so under the current password, which takes focus', async () => {
    const posted = onPost('/api/v1/me/password', () => apiError(403, { code: 'wrong_password' }));
    renderAccount('/account');
    const user = await ready();
    await change(user, 'not my password', 'a much better passphrase');

    const current = screen.getByLabelText('Current password');
    await waitFor(() => {
      expect(current).toHaveAccessibleDescription(WRONG_PASSWORD);
    });
    expect(current).toBeInvalid();
    expect(current).toHaveFocus();
    expect(screen.getByLabelText('New password')).toBeValid();
    expect(screen.queryByText(CHANGED)).not.toBeInTheDocument();
    // What was typed stays, so only the wrong one has to be typed again.
    expect(screen.getByLabelText('New password')).toHaveValue('a much better passphrase');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();

    // The message belongs to that attempt: the next one starts without it.
    await typeInto(user, 'Current password', 'correct horse battery');
    await user.click(screen.getByRole('button', { name: 'Change password' }));
    await waitFor(() => {
      expect(posted).toHaveLength(2);
    });
  });

  it('checks the fields before asking the server', async () => {
    const posted = onPost('/api/v1/me/password', () => HttpResponse.json({}));
    renderAccount('/account');
    const user = await ready();

    await user.click(screen.getByRole('button', { name: 'Change password' }));
    expect(screen.getByLabelText('Current password')).toHaveAccessibleDescription('Enter your current password.');
    expect(screen.getByLabelText('New password')).toHaveAccessibleDescription(/Enter a new password\.$/);
    // Focus goes to the first field that needs attention.
    expect(screen.getByLabelText('Current password')).toHaveFocus();

    await change(user, 'correct horse battery', 'short');
    expect(screen.getByLabelText('New password')).toHaveAccessibleDescription(/That's too short for a password\.$/);
    expect(screen.getByLabelText('Current password')).toBeValid();
    expect(screen.getByLabelText('New password')).toHaveFocus();

    await change(user, 'correct horse battery', 'x'.repeat(129));
    expect(screen.getByLabelText('New password')).toHaveAccessibleDescription(/That's too long for a password\.$/);
    expect(posted).toEqual([]);
  });

  describe('validation_failed (422) from the server', () => {
    it.each(NEW_PASSWORD_CODES)('newPassword %s', async (code, shown) => {
      onPost('/api/v1/me/password', () => apiError(422, { code: 'validation_failed', fields: { newPassword: code } }));
      renderAccount('/account');
      const user = await ready();
      await change(user, 'correct horse battery', 'a much better passphrase');
      const field = screen.getByLabelText('New password');
      await waitFor(() => {
        expect(field).toBeInvalid();
      });
      expect(field).toHaveAccessibleDescription(new RegExp(`${shown.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`));
      expect(field).toHaveFocus();
      expect(screen.queryByText(CHANGED)).not.toBeInTheDocument();
    });

    it('currentPassword required', async () => {
      onPost('/api/v1/me/password', () =>
        apiError(422, { code: 'validation_failed', fields: { currentPassword: 'required' } }),
      );
      renderAccount('/account');
      const user = await ready();
      await change(user, ' ', 'a much better passphrase');
      await waitFor(() => {
        expect(screen.getByLabelText('Current password')).toHaveAccessibleDescription('Enter your current password.');
      });
    });

    it('a field the form does not have: the general message above the form', async () => {
      onPost('/api/v1/me/password', () => apiError(422, { code: 'validation_failed', fields: { other: 'invalid' } }));
      renderAccount('/account');
      const user = await ready();
      await change(user, 'correct horse battery', 'a much better passphrase');
      expect(await screen.findByRole('alert')).toHaveTextContent('Check the highlighted fields.');
    });
  });

  it.each([
    ['rate_limited (429)', 429, 'rate_limited', 'Too many attempts. Try again in 42 seconds.'],
    ['server_busy (503)', 503, 'server_busy', 'The server is busy. Try again in 42 seconds.'],
  ] as const)('%s with a wait: shows it and keeps the button off', async (_name, status, code, shown) => {
    const posted = onPost('/api/v1/me/password', () => apiError(status, { code, retryAfter: 42 }));
    renderAccount('/account');
    const user = await ready();
    await change(user, 'correct horse battery', 'a much better passphrase');
    const alert = await screen.findByRole('alert');
    expect(within(alert).getAllByText(shown).length).toBeGreaterThan(0);
    const submit = screen.getByRole('button', { name: 'Change password' });
    expect(submit).toBeDisabled();
    await user.click(submit);
    expect(posted).toHaveLength(1);
  });

  it.each(SHOWN_ABOVE)('%s: the message above the form', async (_name, status, error, shown) => {
    onPost('/api/v1/me/password', () => apiError(status, error));
    renderAccount('/account');
    const user = await ready();
    await change(user, 'correct horse battery', 'a much better passphrase');
    expect(await screen.findByRole('alert')).toHaveTextContent(shown);
    expect(screen.queryByText(CHANGED)).not.toBeInTheDocument();
    // Nothing is lost: the form can be sent again.
    expect(screen.getByLabelText('New password')).toHaveValue('a much better passphrase');
    expect(screen.getByRole('button', { name: 'Change password' })).toBeEnabled();
  });

  it('no connection: says so above the form', async () => {
    onPost('/api/v1/me/password', networkError);
    renderAccount('/account');
    const user = await ready();
    await change(user, 'correct horse battery', 'a much better passphrase');
    expect(await screen.findByRole('alert')).toHaveTextContent("You're offline. Check your connection.");
  });

  it('unauthenticated (401): the session is gone, so the page goes to the login page', async () => {
    const { session, router } = renderAccount('/account');
    onPost('/api/v1/me/password', () => {
      session.me = null;
      return apiError(401, { code: 'unauthenticated' });
    });
    const user = await ready();
    await change(user, 'correct horse battery', 'a much better passphrase');
    expect(await screen.findByRole('heading', { name: LOGIN_HEADING })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/login');
  });
});

describe('AccountPage: delete account', () => {
  let otherTab: BroadcastChannel;
  let heardByOtherTab: unknown[];

  beforeEach(() => {
    otherTab = new BroadcastChannel(CHANNEL_NAME);
    heardByOtherTab = [];
    otherTab.onmessage = (e: MessageEvent<unknown>) => heardByOtherTab.push(e.data);
  });

  afterEach(() => {
    otherTab.close();
  });

  async function openDialog(user: UserEvent): Promise<HTMLElement> {
    await user.click(screen.getByRole('button', { name: 'Delete account' }));
    const dialog = screen.getByRole('dialog', { name: DELETE_DIALOG });
    expect(dialog).toHaveAttribute('open');
    return dialog;
  }

  async function confirm(user: UserEvent, dialog: HTMLElement, password: string): Promise<void> {
    const field = within(dialog).getByLabelText('Password');
    await user.clear(field);
    if (password !== '') await user.type(field, password);
    await user.click(within(dialog).getByRole('button', { name: 'Delete my account' }));
  }

  it('says what it does, and asks for the password in a dialog that names the account', async () => {
    renderAccount('/account');
    const user = await ready();
    const section = screen.getByRole('region', { name: 'Delete account' });
    expect(
      within(section).getByText(
        "This removes your account from Test server and signs you out everywhere. It can't be undone.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();

    const dialog = await openDialog(user);
    expect(within(dialog).getByText('Enter your password to delete the account alex for good.')).toBeInTheDocument();
    const field = within(dialog).getByLabelText('Password');
    expect(field).toHaveAttribute('autocomplete', 'current-password');
    expect(field).toHaveFocus();
  });

  it('needs the password before it asks the server', async () => {
    const posted = onPost('/api/v1/me/delete', noContent);
    renderAccount('/account');
    const user = await ready();
    const dialog = await openDialog(user);
    await confirm(user, dialog, '');
    expect(within(dialog).getByLabelText('Password')).toHaveAccessibleDescription('Enter a password.');
    expect(within(dialog).getByLabelText('Password')).toHaveFocus();
    expect(posted).toEqual([]);
    expect(dialog).toHaveAttribute('open');
  });

  it('deletes the account: this tab and the others are signed out, and the login page says so', async () => {
    const { session, router, services } = renderAccount('/account');
    const deleted = onPost('/api/v1/me/delete', () => {
      session.me = null;
      return noContent();
    });
    // This tab's side of the sign-out is the logout flow, whose request has nothing left to end.
    const loggedOut = onPost('/api/v1/auth/logout', noContent);
    const user = await ready();
    const dialog = await openDialog(user);
    await confirm(user, dialog, 'correct horse battery');

    expect(await screen.findByRole('heading', { name: LOGIN_HEADING })).toBeInTheDocument();
    expect(deleted).toEqual([{ password: 'correct horse battery' }]);
    // Where the guard sends every signed-out page.
    expect(router.state.location.pathname + router.state.location.search).toBe('/login?next=%2Faccount');
    expect(services.ui.getState().toasts.map((toast) => [toast.kind, toast.message])).toEqual([
      ['success', 'Your account was deleted.'],
    ]);
    await waitFor(() => {
      expect(services.queryClient.getQueryData(queryKeys.me)).toBeNull();
      expect(heardByOtherTab).toEqual([{ type: 'logout' }]);
    });
    expect(loggedOut).toEqual([{}]);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('still signs this tab and the others out when the logout flow cannot reach the server', async () => {
    const { session, router, services } = renderAccount('/account');
    onPost('/api/v1/me/delete', () => {
      session.me = null;
      return noContent();
    });
    onPost('/api/v1/auth/logout', networkError);
    // A resource of the deleted user must not stay in this tab's cache.
    services.queryClient.setQueryData(queryKeys.meSessions, { sessions: [] });
    const user = await ready();
    await confirm(user, await openDialog(user), 'correct horse battery');

    expect(await screen.findByRole('heading', { name: LOGIN_HEADING })).toBeInTheDocument();
    await waitFor(() => {
      expect(heardByOtherTab).toEqual([{ type: 'logout' }]);
    });
    expect(services.queryClient.getQueryData(queryKeys.me)).toBeNull();
    expect(services.queryClient.getQueryData(queryKeys.meSessions)).toBeUndefined();
    expect(router.state.location.pathname).toBe('/login');
  });

  it('wrong_password (403): says so under the field and keeps the dialog open', async () => {
    const { services } = renderAccount('/account');
    onPost('/api/v1/me/delete', () => apiError(403, { code: 'wrong_password' }));
    const user = await ready();
    const dialog = await openDialog(user);
    await confirm(user, dialog, 'not my password');

    const field = within(dialog).getByLabelText('Password');
    await waitFor(() => {
      expect(field).toHaveAccessibleDescription(WRONG_PASSWORD);
    });
    expect(field).toBeInvalid();
    expect(field).toHaveFocus();
    expect(dialog).toHaveAttribute('open');
    expect(within(dialog).getByRole('button', { name: 'Delete my account' })).toBeEnabled();
    expect(services.queryClient.getQueryData(queryKeys.me)).not.toBeNull();
    expect(heardByOtherTab).toEqual([]);
  });

  it('last_admin (409): explains why the only admin cannot leave, and what to do', async () => {
    const { services } = renderAccount('/account', { me: meFixture({ admin: true }) });
    const posted = onPost('/api/v1/me/delete', () => apiError(409, { code: 'last_admin' }));
    const user = await ready();
    const dialog = await openDialog(user);
    await confirm(user, dialog, 'correct horse battery');

    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      "You're the only admin, and this server needs one. Make someone else an admin first, then delete your account.",
    );
    expect(dialog).toHaveAttribute('open');
    expect(within(dialog).getByLabelText('Password')).toBeValid();
    expect(services.queryClient.getQueryData(queryKeys.me)).not.toBeNull();
    expect(posted).toHaveLength(1);
    expect(screen.getByRole('heading', { name: HEADING })).toBeInTheDocument();
  });

  it.each(SHOWN_ABOVE)('%s: the message in the dialog', async (_name, status, error, shown) => {
    onPost('/api/v1/me/delete', () => apiError(status, error));
    renderAccount('/account');
    const user = await ready();
    const dialog = await openDialog(user);
    await confirm(user, dialog, 'correct horse battery');
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(shown);
    expect(dialog).toHaveAttribute('open');
    expect(within(dialog).getByRole('button', { name: 'Delete my account' })).toBeEnabled();
  });

  it('rate_limited (429) with a wait: shows it and keeps the button off', async () => {
    const posted = onPost('/api/v1/me/delete', () => apiError(429, { code: 'rate_limited', retryAfter: 42 }));
    renderAccount('/account');
    const user = await ready();
    const dialog = await openDialog(user);
    await confirm(user, dialog, 'correct horse battery');
    const alert = await within(dialog).findByRole('alert');
    expect(within(alert).getAllByText('Too many attempts. Try again in 42 seconds.').length).toBeGreaterThan(0);
    const submit = within(dialog).getByRole('button', { name: 'Delete my account' });
    expect(submit).toBeDisabled();
    await user.click(submit);
    expect(posted).toHaveLength(1);
  });

  it('Cancel closes it without a request, and the next opening starts empty', async () => {
    const posted = onPost('/api/v1/me/delete', () => apiError(403, { code: 'wrong_password' }));
    renderAccount('/account');
    const user = await ready();
    let dialog = await openDialog(user);
    await confirm(user, dialog, 'not my password');
    await waitFor(() => {
      expect(within(dialog).getByLabelText('Password')).toBeInvalid();
    });

    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(dialog).not.toHaveAttribute('open');
    expect(posted).toHaveLength(1);

    dialog = await openDialog(user);
    const field = within(dialog).getByLabelText('Password');
    expect(field).toHaveValue('');
    expect(field).not.toHaveAttribute('aria-invalid');
    expect(field).not.toHaveAccessibleDescription();
    expect(field).toHaveFocus();
  });
});
