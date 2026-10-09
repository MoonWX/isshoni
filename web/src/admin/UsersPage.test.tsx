// /admin/users (05 §15.3) against MSW: the list, each row action with the request it sends (03 §12.4.8), what the
// own row leaves out, and the error codes of 03 §12.3 #30–#33.
import { onlineManager } from '@tanstack/react-query';
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';

import { formatRelativeTime } from '../lib/time';
import type { AdminUser } from '../protocol/api.gen';
import { apiError } from '../test/msw';
import {
  adminUser,
  dialog,
  gate,
  networkError,
  noContent,
  on,
  pickAction,
  renderAdmin,
  rowOf,
  selfUser,
  serve,
  toasts,
  typeInto,
  without,
} from './testing/harness';

const SAM = 'b8f2n4r6t0vz';
/** A reset link as POST …/password-reset answers it. */
const LINK = 'https://watch.example.com/reset#EXAMPLEresetTOKEN0123456789abcdef';

/** GET /api/v1/admin/users over a list the test changes. */
function usersApi(initial: AdminUser[] = [selfUser(), adminUser()]) {
  const state = { users: initial };
  const gets = serve('/api/v1/admin/users', () => ({ users: state.users }));
  return { state, gets };
}

/** PATCH /api/v1/admin/users/:id that applies the patch to the list, like the server. */
function patchWorks(state: { users: AdminUser[] }) {
  return on('patch', '/api/v1/admin/users/:id', ({ body, params }) => {
    // The password is checked, not stored.
    const patch = without(body as Partial<AdminUser> & { currentPassword?: string }, 'currentPassword');
    state.users = state.users.map((u) => (u.id === params['id'] ? { ...u, ...patch } : u));
    return HttpResponse.json({ user: state.users.find((u) => u.id === params['id']) });
  });
}

async function openUsers(initial?: AdminUser[]) {
  const api = usersApi(initial);
  const page = renderAdmin({ path: '/admin/users' });
  await screen.findByRole('table', { name: 'Users' });
  return { ...api, ...page };
}

describe('UsersPage', () => {
  it('lists every account with its role, status, origin and last sign of life', async () => {
    await openUsers([
      selfUser(),
      adminUser(),
      adminUser({ id: 'd1d1d1d1d1d1', username: 'kim', status: 'disabled', createdVia: 'signup', resetPending: true }),
      adminUser({ id: 'c2c2c2c2c2c2', username: 'bo', createdVia: 'cli', online: true }),
    ]);
    const headers = screen.getAllByRole('columnheader').map((th) => th.textContent);
    expect(headers).toEqual(['User', 'Role', 'Status', 'Joined', 'Last seen']);

    const own = within(rowOf(/^admin/));
    expect(own.getByRole('rowheader')).toHaveTextContent('adminYou');
    expect(own.getByText('Admin')).toBeInTheDocument();
    expect(own.getByText('Set up the server')).toBeInTheDocument();
    expect(own.getByText('Online now')).toBeInTheDocument();

    const sam = within(rowOf(/^sam/));
    expect(sam.getByText('Member')).toBeInTheDocument();
    expect(sam.getByText('Active')).toBeInTheDocument();
    expect(sam.getByText('Invited by admin')).toBeInTheDocument();
    // Not online: when they were last seen, as a time element. The text is relative to today ("6 days ago" one day
    // is "last week" the next), so it is compared with the formatter's own answer, not with a fixed phrase.
    const lastSeen = sam.getByText(formatRelativeTime(new Date('2026-10-02T09:30:00.000Z'), 'en'));
    expect(lastSeen.closest('time')).toHaveAttribute('datetime', '2026-10-02T09:30:00.000Z');

    const kim = within(rowOf(/^kim/));
    expect(kim.getByText('Disabled')).toBeInTheDocument();
    expect(kim.getByText("Has a reset link that wasn't used yet")).toBeInTheDocument();
    expect(kim.getByText('Asked for an account')).toBeInTheDocument();
    expect(within(rowOf(/^bo/)).getByText('Created on the server')).toBeInTheDocument();
  });

  it('says "Never" for an account that was never seen', async () => {
    await openUsers([selfUser(), without(adminUser(), 'lastSeenAt')]);
    expect(within(rowOf(/^sam/)).getByText('Never')).toBeInTheDocument();
  });

  it('offers every action on another account', async () => {
    const { user } = await openUsers();
    await user.click(screen.getByRole('button', { name: 'Actions for sam' }));
    const menu = within(screen.getByRole('dialog', { name: 'Actions for sam' }));
    expect(menu.getAllByRole('button').map((b) => b.textContent)).toEqual([
      'Rename',
      'Make admin',
      'Disable',
      'Create reset link',
      'Sign out everywhere',
      'Delete',
    ]);
  });

  it('leaves disable, delete and the reset link off the own row', async () => {
    const { user } = await openUsers();
    await user.click(screen.getByRole('button', { name: 'Actions for admin' }));
    const menu = within(screen.getByRole('dialog', { name: 'Actions for admin' }));
    expect(menu.getAllByRole('button').map((b) => b.textContent)).toEqual([
      'Rename',
      'Remove admin',
      'Sign out everywhere',
    ]);
  });

  it('sends a pending sign-up to the Approvals page instead of offering actions', async () => {
    await openUsers([selfUser(), adminUser({ username: 'newbie', status: 'pending', createdVia: 'signup' })]);
    const row = within(rowOf(/^newbie/));
    expect(row.getByText('Waiting for approval')).toBeInTheDocument();
    expect(row.getByRole('link', { name: 'Review' })).toHaveAttribute('href', '/admin/approvals');
    expect(row.queryByRole('button')).not.toBeInTheDocument();
  });

  describe('rename', () => {
    it('sends the new username and shows it', async () => {
      const { user, state, services } = await openUsers();
      const patched = patchWorks(state);
      await pickAction(user, 'sam', 'Rename');
      const field = within(dialog('Rename sam')).getByLabelText('Username');
      expect(field).toHaveValue('sam');
      expect(field).toHaveFocus();
      await typeInto(user, field, 'samuel');
      await user.click(within(dialog('Rename sam')).getByRole('button', { name: 'Rename' }));
      expect(await screen.findByRole('row', { name: /^samuel/ })).toBeInTheDocument();
      expect(patched).toEqual([
        expect.objectContaining({ url: `/api/v1/admin/users/${SAM}`, body: { username: 'samuel' } }),
      ]);
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(toasts(services)).toEqual(['sam is now samuel.']);
    });

    it('submits with Enter', async () => {
      const { user, state } = await openUsers();
      const patched = patchWorks(state);
      await pickAction(user, 'sam', 'Rename');
      await typeInto(user, within(dialog('Rename sam')).getByLabelText('Username'), 'sammy{Enter}');
      expect(await screen.findByRole('row', { name: /^sammy/ })).toBeInTheDocument();
      expect(patched).toHaveLength(1);
    });

    it('sends nothing for the same name or an empty one', async () => {
      const { user, state } = await openUsers();
      const patched = patchWorks(state);
      await pickAction(user, 'sam', 'Rename');
      const field = within(dialog('Rename sam')).getByLabelText('Username');
      await typeInto(user, field, '');
      await user.click(within(dialog('Rename sam')).getByRole('button', { name: 'Rename' }));
      expect(field).toHaveAccessibleDescription(expect.stringContaining('Enter a username.'));
      expect(field).toBeInvalid();
      expect(field).toHaveFocus();
      await typeInto(user, field, 'sam');
      await user.click(within(dialog('Rename sam')).getByRole('button', { name: 'Rename' }));
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(patched).toEqual([]);
    });

    it.each([
      ['username_taken', 409, { code: 'username_taken' }, 'That username is taken.'],
      [
        'validation_failed',
        422,
        { code: 'validation_failed', fields: { username: 'reserved' } },
        'That username is reserved. Pick another.',
      ],
    ] as const)('shows %s under the field', async (_name, status, error, shown) => {
      const { user } = await openUsers();
      on('patch', '/api/v1/admin/users/:id', () => apiError(status, error));
      await pickAction(user, 'sam', 'Rename');
      const field = within(dialog('Rename sam')).getByLabelText('Username');
      await typeInto(user, field, 'kim');
      await user.click(within(dialog('Rename sam')).getByRole('button', { name: 'Rename' }));
      await waitFor(() => {
        expect(field).toHaveAccessibleDescription(expect.stringContaining(shown));
      });
      expect(field).toBeInvalid();
      expect(dialog('Rename sam')).toBeInTheDocument();
    });

    it.each([
      ['user_not_found', 404, { code: 'user_not_found' }, "That user doesn't exist anymore."],
      [
        'internal',
        500,
        { code: 'internal', requestId: 'req-1' },
        'Something went wrong on the server. Reference: req-1',
      ],
      ['a code this build does not know', 418, { code: 'brand_new' }, 'Something went wrong (brand_new).'],
    ] as const)('shows %s above the fields', async (_name, status, error, shown) => {
      const { user } = await openUsers();
      on('patch', '/api/v1/admin/users/:id', () => apiError(status, error));
      await pickAction(user, 'sam', 'Rename');
      await typeInto(user, within(dialog('Rename sam')).getByLabelText('Username'), 'kim');
      await user.click(within(dialog('Rename sam')).getByRole('button', { name: 'Rename' }));
      expect(await within(dialog('Rename sam')).findByRole('alert')).toHaveTextContent(shown);
    });

    it('says so when the server is unreachable, and Cancel closes', async () => {
      const { user } = await openUsers();
      on('patch', '/api/v1/admin/users/:id', networkError);
      await pickAction(user, 'sam', 'Rename');
      await typeInto(user, within(dialog('Rename sam')).getByLabelText('Username'), 'kim');
      await user.click(within(dialog('Rename sam')).getByRole('button', { name: 'Rename' }));
      expect(await within(dialog('Rename sam')).findByRole('alert')).toHaveTextContent("You're offline.");
      await user.click(within(dialog('Rename sam')).getByRole('button', { name: 'Cancel' }));
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
  });

  describe('make and remove admin', () => {
    it("asks for the admin's password and sends it with the new role", async () => {
      const { user, state, services } = await openUsers();
      const patched = patchWorks(state);
      await pickAction(user, 'sam', 'Make admin');
      const box = within(dialog('Make sam an admin?'));
      await user.type(box.getByLabelText('Your password'), 'correct horse');
      await user.click(box.getByRole('button', { name: 'Make admin' }));
      await waitFor(() => {
        expect(within(rowOf(/^sam/)).getByText('Admin')).toBeInTheDocument();
      });
      expect(patched.map((p) => p.body)).toEqual([{ role: 'admin', currentPassword: 'correct horse' }]);
      expect(toasts(services)).toEqual(['sam is now an admin.']);
    });

    it('sends nothing without the password', async () => {
      const { user, state } = await openUsers();
      const patched = patchWorks(state);
      await pickAction(user, 'sam', 'Make admin');
      const box = within(dialog('Make sam an admin?'));
      await user.click(box.getByRole('button', { name: 'Make admin' }));
      // The field's own text (fieldErrors.currentPassword.required), not the admin pages' generic one.
      expect(box.getByLabelText('Your password')).toHaveAccessibleDescription('Enter your current password.');
      expect(patched).toEqual([]);
    });

    it('shows wrong_password under the password field', async () => {
      const { user } = await openUsers();
      on('patch', '/api/v1/admin/users/:id', () => apiError(403, { code: 'wrong_password' }));
      await pickAction(user, 'sam', 'Make admin');
      const box = within(dialog('Make sam an admin?'));
      const password = box.getByLabelText('Your password');
      await user.type(password, 'nope');
      await user.click(box.getByRole('button', { name: 'Make admin' }));
      await waitFor(() => {
        expect(password).toHaveAccessibleDescription("Your current password isn't right.");
      });
      expect(password).toBeInvalid();
      expect(password).toHaveFocus();
    });

    it('removes admin without a password', async () => {
      const { user, state, services } = await openUsers([selfUser(), adminUser({ role: 'admin' })]);
      const patched = patchWorks(state);
      await pickAction(user, 'sam', 'Remove admin');
      const box = within(dialog('Remove sam as admin?'));
      expect(box.getByText('sam keeps the account, as a member.')).toBeInTheDocument();
      expect(box.queryByLabelText('Your password')).not.toBeInTheDocument();
      await user.click(box.getByRole('button', { name: 'Remove admin' }));
      await waitFor(() => {
        expect(within(rowOf(/^sam/)).getByText('Member')).toBeInTheDocument();
      });
      expect(patched.map((p) => p.body)).toEqual([{ role: 'user' }]);
      expect(toasts(services)).toEqual(['sam is no longer an admin.']);
    });

    it('explains last_admin when the last admin would step down', async () => {
      const { user } = await openUsers();
      on('patch', '/api/v1/admin/users/:id', () => apiError(409, { code: 'last_admin' }));
      await pickAction(user, 'admin', 'Remove admin');
      const box = within(dialog('Remove admin as admin?'));
      expect(box.getByText(/lose these admin pages right away/)).toBeInTheDocument();
      await user.click(box.getByRole('button', { name: 'Remove admin' }));
      expect(await box.findByRole('alert')).toHaveTextContent('The server needs at least one active admin.');
    });

    it('takes the admin pages away from an admin who stepped down', async () => {
      const { user, state } = await openUsers([selfUser(), adminUser({ role: 'admin' })]);
      on('patch', '/api/v1/admin/users/:id', ({ params }) => {
        state.users = state.users.map((u) => (u.id === params['id'] ? { ...u, role: 'user' } : u));
        // The server's answer to GET /api/v1/me changes with the role.
        on('get', '/api/v1/me', () =>
          HttpResponse.json({
            user: { id: 'a1b2c3d4e5f6', username: 'admin', role: 'user' },
            permissions: { admin: false, createInvites: false },
          }),
        );
        on('get', '/api/v1/admin/users', () => apiError(403, { code: 'forbidden' }));
        return HttpResponse.json({ user: state.users[0] });
      });
      await pickAction(user, 'admin', 'Remove admin');
      await user.click(within(dialog('Remove admin as admin?')).getByRole('button', { name: 'Remove admin' }));
      expect(await screen.findByRole('heading', { name: 'Nothing here' })).toBeInTheDocument();
    });
  });

  describe('disable and enable', () => {
    it('disables after a question', async () => {
      const { user, state, services } = await openUsers();
      const patched = patchWorks(state);
      await pickAction(user, 'sam', 'Disable');
      const box = within(dialog('Disable sam?'));
      expect(box.getByText(/signed out everywhere and can't log in/)).toBeInTheDocument();
      await user.click(box.getByRole('button', { name: 'Disable' }));
      await waitFor(() => {
        expect(within(rowOf(/^sam/)).getByText('Disabled')).toBeInTheDocument();
      });
      expect(patched.map((p) => p.body)).toEqual([{ status: 'disabled' }]);
      expect(toasts(services)).toEqual(['sam is disabled.']);
    });

    it('enables a disabled account at once', async () => {
      const { user, state, services } = await openUsers([selfUser(), adminUser({ status: 'disabled' })]);
      const patched = patchWorks(state);
      await pickAction(user, 'sam', 'Enable');
      await waitFor(() => {
        expect(within(rowOf(/^sam/)).getByText('Active')).toBeInTheDocument();
      });
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(patched.map((p) => p.body)).toEqual([{ status: 'active' }]);
      expect(toasts(services)).toEqual(['sam can log in again.']);
    });

    it('says why enabling failed', async () => {
      const { user, services } = await openUsers([selfUser(), adminUser({ status: 'disabled' })]);
      on('patch', '/api/v1/admin/users/:id', () => apiError(404, { code: 'user_not_found' }));
      await pickAction(user, 'sam', 'Enable');
      await waitFor(() => {
        expect(toasts(services)).toEqual(["That user doesn't exist anymore."]);
      });
    });
  });

  describe('reset link', () => {
    it('creates the link for a member without a password, and shows it once', async () => {
      const { user, gets } = await openUsers();
      const posted = on('post', '/api/v1/admin/users/:id/password-reset', () =>
        HttpResponse.json({ url: LINK, expiresAt: '2026-10-04T12:00:00.000Z' }, { status: 201 }),
      );
      await pickAction(user, 'sam', 'Create reset link');
      const ask = within(dialog('Create a reset link for sam?'));
      expect(ask.getByText(/password stops working right away/)).toBeInTheDocument();
      expect(ask.queryByLabelText('Your password')).not.toBeInTheDocument();
      const listed = gets.length;
      await user.click(ask.getByRole('button', { name: 'Create link' }));

      const shown = within(await screen.findByRole('dialog', { name: 'Reset link for sam' }));
      const link = shown.getByLabelText('Reset link');
      expect(link).toHaveValue(LINK);
      expect(link).toHaveAttribute('readonly');
      // Focus is on the link, ready to copy.
      expect(link).toHaveFocus();
      expect(link).toHaveAccessibleDescription(
        expect.stringMatching(/send it to sam privately.*shown only once.*works until/),
      );
      expect(posted).toEqual([expect.objectContaining({ url: `/api/v1/admin/users/${SAM}/password-reset`, body: {} })]);
      // The user's sessions are gone and a reset is pending: the list was asked for again.
      await waitFor(() => {
        expect(gets.length).toBeGreaterThan(listed);
      });

      await user.click(shown.getByRole('button', { name: 'Copy link' }));
      expect(await shown.findByRole('button', { name: 'Copied' })).toBeInTheDocument();
      await expect(navigator.clipboard.readText()).resolves.toBe(LINK);

      // Only "Done" closes it: Esc would lose a link that can't be shown again.
      expect(shown.queryByRole('button', { name: 'Close' })).not.toBeInTheDocument();
      // What the browser sends a <dialog> for Esc.
      fireEvent(screen.getByRole('dialog', { name: 'Reset link for sam' }), new Event('cancel', { cancelable: true }));
      expect(screen.getByRole('dialog', { name: 'Reset link for sam' })).toBeInTheDocument();
      await user.click(shown.getByRole('button', { name: 'Done' }));
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(document.body).not.toHaveTextContent('EXAMPLEresetTOKEN');
    });

    it('selects the link when the browser refuses to copy', async () => {
      const { user } = await openUsers();
      on('post', '/api/v1/admin/users/:id/password-reset', () =>
        HttpResponse.json({ url: LINK, expiresAt: '2026-10-04T12:00:00.000Z' }, { status: 201 }),
      );
      await pickAction(user, 'sam', 'Create reset link');
      await user.click(within(dialog('Create a reset link for sam?')).getByRole('button', { name: 'Create link' }));
      const shown = within(await screen.findByRole('dialog', { name: 'Reset link for sam' }));
      vi.spyOn(navigator.clipboard, 'writeText').mockRejectedValue(new DOMException('no', 'NotAllowedError'));
      await user.click(shown.getByRole('button', { name: 'Copy link' }));
      expect(await shown.findByText(/Couldn't copy for you/)).toBeInTheDocument();
      const link = shown.getByLabelText<HTMLInputElement>('Reset link');
      expect(link).toHaveFocus();
      expect([link.selectionStart, link.selectionEnd]).toEqual([0, LINK.length]);
    });

    it("asks for the admin's password when the target is an admin", async () => {
      const { user } = await openUsers([selfUser(), adminUser({ role: 'admin' })]);
      let attempt = 0;
      const posted = on('post', '/api/v1/admin/users/:id/password-reset', () =>
        ++attempt === 1
          ? apiError(403, { code: 'wrong_password' })
          : HttpResponse.json({ url: LINK, expiresAt: '2026-10-04T12:00:00.000Z' }, { status: 201 }),
      );
      await pickAction(user, 'sam', 'Create reset link');
      const ask = within(dialog('Create a reset link for sam?'));
      expect(ask.getByText('sam is an admin, so enter your password to confirm.')).toBeInTheDocument();
      const password = ask.getByLabelText('Your password');

      await user.click(ask.getByRole('button', { name: 'Create link' }));
      expect(password).toHaveAccessibleDescription('Enter your current password.');
      expect(posted).toEqual([]);

      await user.type(password, 'nope');
      await user.click(ask.getByRole('button', { name: 'Create link' }));
      await waitFor(() => {
        expect(password).toHaveAccessibleDescription("Your current password isn't right.");
      });

      await typeInto(user, password, 'correct horse');
      await user.click(ask.getByRole('button', { name: 'Create link' }));
      expect(await screen.findByRole('dialog', { name: 'Reset link for sam' })).toBeInTheDocument();
      expect(posted.map((p) => p.body)).toEqual([{ currentPassword: 'nope' }, { currentPassword: 'correct horse' }]);
    });

    it('asks for the password after all when the user became an admin since the list loaded', async () => {
      const { user, state, gets } = await openUsers();
      const posted = on('post', '/api/v1/admin/users/:id/password-reset', ({ body }) =>
        (body as { currentPassword?: string }).currentPassword === 'correct horse'
          ? HttpResponse.json({ url: LINK, expiresAt: '2026-10-04T12:00:00.000Z' }, { status: 201 })
          : apiError(403, { code: 'wrong_password' }),
      );
      // Another admin made sam an admin; this page still shows a member.
      state.users = [selfUser(), adminUser({ role: 'admin' })];
      await pickAction(user, 'sam', 'Create reset link');
      const ask = within(dialog('Create a reset link for sam?'));
      expect(ask.queryByLabelText('Your password')).not.toBeInTheDocument();
      const listed = gets.length;
      await user.click(ask.getByRole('button', { name: 'Create link' }));

      // The server wants the password: the field shows up with the reason, focused, and without an error (no
      // password was sent, so none was wrong).
      const password = await ask.findByLabelText('Your password');
      expect(ask.getByText('sam is an admin, so enter your password to confirm.')).toBeInTheDocument();
      expect(password).toHaveFocus();
      expect(password).not.toHaveAttribute('aria-invalid', 'true');
      expect(password).not.toHaveAccessibleDescription();
      expect(ask.queryByRole('alert')).not.toBeInTheDocument();
      // The list that was behind is asked for again.
      await waitFor(() => {
        expect(gets.length).toBeGreaterThan(listed);
      });
      await waitFor(() => {
        expect(within(rowOf(/^sam/)).getByText('Admin')).toBeInTheDocument();
      });

      // From here on it is the dialog for an admin: the password is required, and a wrong one shows under it.
      await user.click(ask.getByRole('button', { name: 'Create link' }));
      expect(password).toHaveAccessibleDescription('Enter your current password.');
      await user.type(password, 'nope');
      await user.click(ask.getByRole('button', { name: 'Create link' }));
      await waitFor(() => {
        expect(password).toHaveAccessibleDescription("Your current password isn't right.");
      });
      await typeInto(user, password, 'correct horse');
      await user.click(ask.getByRole('button', { name: 'Create link' }));
      expect(await screen.findByRole('dialog', { name: 'Reset link for sam' })).toBeInTheDocument();
      expect(posted.map((p) => p.body)).toEqual([
        {},
        { currentPassword: 'nope' },
        { currentPassword: 'correct horse' },
      ]);
    });
  });

  describe('while a change is on its way', () => {
    it("can't be left: no Cancel, no Close, no Esc, and the reset link still shows when the answer arrives", async () => {
      const { user } = await openUsers();
      const answer = gate();
      const posted = on('post', '/api/v1/admin/users/:id/password-reset', async () => {
        await answer.opened;
        return HttpResponse.json({ url: LINK, expiresAt: '2026-10-04T12:00:00.000Z' }, { status: 201 });
      });
      await pickAction(user, 'sam', 'Create reset link');
      const box = dialog('Create a reset link for sam?');
      const ask = within(box);
      expect(ask.getByRole('button', { name: 'Cancel' })).toBeEnabled();
      expect(ask.getByRole('button', { name: 'Close' })).toBeInTheDocument();
      await user.click(ask.getByRole('button', { name: 'Create link' }));
      await waitFor(() => {
        expect(posted).toHaveLength(1);
      });

      // The server has already cleared sam's password: the link must not get lost.
      await waitFor(() => {
        expect(ask.getByRole('button', { name: 'Cancel' })).toBeDisabled();
      });
      expect(ask.getByRole('button', { name: 'Create link' })).toHaveAttribute('aria-busy', 'true');
      expect(ask.queryByRole('button', { name: 'Close' })).not.toBeInTheDocument();
      // What the browser sends a <dialog> for Esc.
      fireEvent(box, new Event('cancel', { cancelable: true }));
      // A click beside the dialog (on its backdrop) arrives at the <dialog> element itself.
      fireEvent.click(box);
      expect(box).toBeInTheDocument();

      answer.open();
      const shown = within(await screen.findByRole('dialog', { name: 'Reset link for sam' }));
      expect(shown.getByLabelText('Reset link')).toHaveValue(LINK);
    });

    it('lets go again when the change failed', async () => {
      const { user } = await openUsers();
      const answer = gate();
      on('patch', '/api/v1/admin/users/:id', async () => {
        await answer.opened;
        return apiError(409, { code: 'last_admin' });
      });
      await pickAction(user, 'sam', 'Disable');
      const box = within(dialog('Disable sam?'));
      await user.click(box.getByRole('button', { name: 'Disable' }));
      await waitFor(() => {
        expect(box.getByRole('button', { name: 'Cancel' })).toBeDisabled();
      });
      answer.open();
      expect(await box.findByRole('alert')).toHaveTextContent('The server needs at least one active admin.');
      expect(await box.findByRole('button', { name: 'Close' })).toBeInTheDocument();
      await user.click(box.getByRole('button', { name: 'Cancel' }));
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    it('sends the change at once while the browser says it is offline, and says why it failed', async () => {
      const { user } = await openUsers();
      const patched = on('patch', '/api/v1/admin/users/:id', networkError);
      onlineManager.setOnline(false);
      try {
        await pickAction(user, 'sam', 'Disable');
        const box = within(dialog('Disable sam?'));
        await user.click(box.getByRole('button', { name: 'Disable' }));
        // Not held back until the connection returns, with a dialog that can't be left meanwhile.
        expect(await box.findByRole('alert')).toHaveTextContent("You're offline.");
        expect(patched).toHaveLength(1);
        await waitFor(() => {
          expect(box.getByRole('button', { name: 'Cancel' })).toBeEnabled();
        });
      } finally {
        onlineManager.setOnline(true);
      }
    });
  });

  it('signs a user out everywhere and says where', async () => {
    const { user, services } = await openUsers();
    const posted = on('post', '/api/v1/admin/users/:id/sign-out', () => HttpResponse.json({ sessions: 2, devices: 1 }));
    await pickAction(user, 'sam', 'Sign out everywhere');
    await user.click(within(dialog('Sign sam out everywhere?')).getByRole('button', { name: 'Sign out' }));
    await waitFor(() => {
      expect(toasts(services)).toEqual(['sam was signed out in 3 places.']);
    });
    expect(posted).toEqual([expect.objectContaining({ url: `/api/v1/admin/users/${SAM}/sign-out`, body: {} })]);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('says when the user was not signed in anywhere', async () => {
    const { user, services } = await openUsers();
    on('post', '/api/v1/admin/users/:id/sign-out', () => HttpResponse.json({ sessions: 0, devices: 0 }));
    await pickAction(user, 'sam', 'Sign out everywhere');
    await user.click(within(dialog('Sign sam out everywhere?')).getByRole('button', { name: 'Sign out' }));
    await waitFor(() => {
      expect(toasts(services)).toEqual(["sam wasn't signed in anywhere."]);
    });
  });

  describe('delete', () => {
    it('deletes after a question', async () => {
      const { user, state, services } = await openUsers();
      const deleted = on('delete', '/api/v1/admin/users/:id', ({ params }) => {
        state.users = state.users.filter((u) => u.id !== params['id']);
        return noContent();
      });
      await pickAction(user, 'sam', 'Delete');
      const box = within(dialog('Delete sam?'));
      expect(box.getByText(/can't be undone/)).toBeInTheDocument();
      // Picking from the menu left focus on its button, which is where a browser returns it when a dialog closes.
      expect(screen.getByRole('button', { name: 'Actions for sam' })).toHaveFocus();
      expect(screen.queryByRole('dialog', { name: 'Actions for sam' })).not.toBeInTheDocument();
      await user.click(box.getByRole('button', { name: 'Delete account' }));
      await waitFor(() => {
        expect(screen.queryByRole('row', { name: /^sam/ })).not.toBeInTheDocument();
      });
      expect(deleted.map((d) => d.url)).toEqual([`/api/v1/admin/users/${SAM}`]);
      expect(toasts(services)).toEqual(['sam was deleted.']);
      // The menu that opened the dialog is gone with the row: focus is on the page's heading, not nowhere.
      expect(screen.getByRole('heading', { level: 1, name: 'Users' })).toHaveFocus();
    });

    it.each([
      ['last_admin', 'The server needs at least one active admin.'],
      ['self_action_forbidden', 'Change your own account on the Account page.'],
    ])('explains %s', async (code, shown) => {
      const { user } = await openUsers();
      on('delete', '/api/v1/admin/users/:id', () => apiError(409, { code }));
      await pickAction(user, 'sam', 'Delete');
      const box = within(dialog('Delete sam?'));
      await user.click(box.getByRole('button', { name: 'Delete account' }));
      expect(await box.findByRole('alert')).toHaveTextContent(shown);
      expect(rowOf(/^sam/)).toBeInTheDocument();
    });
  });

  it('says why the list could not load, and tries again', async () => {
    let fail = true;
    on('get', '/api/v1/admin/users', () =>
      fail ? apiError(500, { code: 'internal', requestId: 'req-9' }) : HttpResponse.json({ users: [selfUser()] }),
    );
    const { user } = renderAdmin({ path: '/admin/users' });
    expect(await screen.findByRole('alert')).toHaveTextContent(
      "Couldn't load this. Something went wrong on the server. Reference: req-9",
    );
    fail = false;
    await user.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('row', { name: /^admin/ })).toBeInTheDocument();
  });
});
