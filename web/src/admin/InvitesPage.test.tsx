// /admin/invites (05 §15.3, 03 §7.9, §12.4.5) against MSW: creating an invite (the link shows once), the list with
// its client-side filter, revoking, and what a member sees.
import { screen, waitFor, within } from '@testing-library/react';
import { HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import type { Invite } from '../protocol/api.gen';
import { apiError, meFixture } from '../test/msw';
import {
  dialog,
  gate,
  invite,
  noContent,
  on,
  renderAdmin,
  rowOf,
  serve,
  settingsResponse,
  toasts,
  without,
} from './testing/harness';

const LINK = 'https://watch.example.com/invite#EXAMPLEinviteTOKEN0123456789abcd';

const EXPIRED = invite({ id: 'e1e1e1e1e1e1', note: 'old one', state: 'expired' });
const USED_UP = invite({
  id: 'u2u2u2u2u2u2',
  note: '',
  state: 'used_up',
  maxUses: 2,
  uses: 2,
  redeemedBy: [
    { id: 'b8f2n4r6t0vz', username: 'sam' },
    { id: 'k1k1k1k1k1k1', username: 'kim' },
  ],
});
const REVOKED = invite({ id: 'r3r3r3r3r3r3', note: 'oops', state: 'revoked' });

interface Options {
  invites?: Invite[];
  member?: boolean;
  registration?: 'invite' | 'approval' | 'closed';
  /** GET /api/v1/admin/settings; a member's page never asks for it. */
  settings?: ReturnType<typeof settingsResponse>;
}

async function openInvites({ invites = [invite()], member = false, registration = 'invite', settings }: Options = {}) {
  const state = { invites };
  const gets = serve('/api/v1/invites', () => ({ invites: state.invites }));
  const settingsGets = on('get', '/api/v1/admin/settings', () =>
    member ? apiError(403, { code: 'forbidden' }) : HttpResponse.json(settings ?? settingsResponse()),
  );
  const page = renderAdmin({
    path: '/admin/invites',
    info: { registration },
    ...(member ? { me: meFixture({ createInvites: true }) } : {}),
  });
  await screen.findByRole('heading', { level: 1, name: 'Invites' });
  await waitFor(() => {
    expect(gets.length).toBeGreaterThan(0);
  });
  return { state, gets, settingsGets, ...page };
}

/** The texts of a <select>'s options, in order. */
function optionsOf(select: HTMLElement): (string | null)[] {
  return within(select)
    .getAllByRole('option')
    .map((o) => o.textContent);
}

/** The admin's form once GET /api/v1/admin/settings has arrived: both selects show a value. */
async function defaultsShown(): Promise<void> {
  await waitFor(() => {
    expect(screen.getByLabelText('Expires after')).not.toHaveValue('');
  });
}

/** POST /api/v1/invites that adds the invite to the list and answers with its link. */
function createWorks(state: { invites: Invite[] }) {
  return on('post', '/api/v1/invites', ({ body }) => {
    const { note = '', maxUses = 10 } = body as { note?: string; maxUses?: number };
    const created = invite({ id: 'n9n9n9n9n9n9', note, maxUses });
    state.invites = [created, ...state.invites];
    return HttpResponse.json({ invite: created, url: LINK }, { status: 201 });
  });
}

describe('InvitesPage', () => {
  it('asks for every state and hides the invites that no longer work until the box is ticked', async () => {
    const { user, gets } = await openInvites({ invites: [invite(), EXPIRED, USED_UP, REVOKED] });
    await screen.findByRole('table', { name: 'Invites' });
    expect(gets[0]?.url).toBe('/api/v1/invites?state=all');
    expect(screen.getAllByRole('columnheader').map((th) => th.textContent)).toEqual([
      'Invite',
      'Created by',
      'Used',
      'Expires',
      'State',
    ]);
    // Only the active invite shows.
    expect(screen.getAllByRole('row')).toHaveLength(2);
    const active = within(rowOf(/^for Sam/));
    expect(active.getByText('admin')).toBeInTheDocument();
    expect(active.getByText('0 of 10')).toBeInTheDocument();
    expect(active.getByText('Active')).toBeInTheDocument();
    expect(active.getByRole('button', { name: 'Revoke the invite "for Sam"' })).toBeInTheDocument();

    await user.click(screen.getByRole('checkbox', { name: 'Show invites that no longer work (3)' }));
    expect(screen.getAllByRole('row')).toHaveLength(5);
    expect(within(rowOf(/^old one/)).getByText('Expired')).toBeInTheDocument();
    const usedUp = within(rowOf(/^No note/));
    expect(usedUp.getByText('Used up')).toBeInTheDocument();
    expect(usedUp.getByText('2 of 2')).toBeInTheDocument();
    expect(usedUp.getByText('By sam and kim')).toBeInTheDocument();
    const revoked = within(rowOf(/^oops/));
    expect(revoked.getByText('Revoked')).toBeInTheDocument();
    // An invite that no longer works can't be revoked.
    expect(revoked.queryByRole('button')).not.toBeInTheDocument();
    // No request went out for the filter: it is client-side.
    expect(gets).toHaveLength(1);
  });

  it('names an invite whose creator is gone, or that came from the command line, "Unknown"', async () => {
    await openInvites({ invites: [without(invite(), 'createdBy')] });
    expect(within(await screen.findByRole('row', { name: /^for Sam/ })).getByText('Unknown')).toBeInTheDocument();
  });

  it('says so when there are no invites, or none that work', async () => {
    const { state, services } = await openInvites({ invites: [] });
    expect(await screen.findByText('No invite links yet.')).toBeInTheDocument();
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument();
    // The only invite expired; the list hears about it (an `invalidate` message, a refetch on focus).
    state.invites = [EXPIRED];
    await services.queryClient.invalidateQueries({ queryKey: ['invites'] });
    expect(await screen.findByText('No invite link works right now.')).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'Show invites that no longer work (1)' })).not.toBeChecked();
  });

  describe('create', () => {
    it('starts with 7 days and 10 uses, and offers the choices of the spec', async () => {
      await openInvites();
      const expires = screen.getByLabelText('Expires after');
      const uses = screen.getByLabelText('Can be used');
      await waitFor(() => {
        expect(expires).toHaveValue('168');
      });
      expect(uses).toHaveValue('10');
      expect(optionsOf(expires)).toEqual(['1 hour', '1 day', '7 days', '30 days']);
      expect(optionsOf(uses)).toEqual(['1 time', '10 times', '50 times']);
    });

    it("starts with the server's invite defaults when an admin changed them, and keeps them on offer", async () => {
      const { user, state } = await openInvites({
        settings: settingsResponse({ inviteDefaultTtlHours: 48, inviteDefaultMaxUses: 3 }),
      });
      const expires = screen.getByLabelText('Expires after');
      await waitFor(() => {
        expect(expires).toHaveValue('48');
      });
      const expiryOptions = ['1 hour', '1 day', '2 days', '7 days', '30 days'];
      expect(optionsOf(expires)).toEqual(expiryOptions);
      const uses = screen.getByLabelText('Can be used');
      expect(uses).toHaveValue('3');
      const usesOptions = ['1 time', '3 times', '10 times', '50 times'];
      expect(optionsOf(uses)).toEqual(usesOptions);

      // Picking something else doesn't take the server's default off the list: it can be picked again.
      await user.selectOptions(expires, '30 days');
      await user.selectOptions(uses, '50 times');
      expect(optionsOf(expires)).toEqual(expiryOptions);
      expect(optionsOf(uses)).toEqual(usesOptions);
      await user.selectOptions(expires, '2 days');
      await user.selectOptions(uses, '3 times');
      expect(expires).toHaveValue('48');
      expect(uses).toHaveValue('3');

      // An admin's form sends what it shows.
      const posted = createWorks(state);
      await user.click(screen.getByRole('button', { name: 'Create link' }));
      await screen.findByRole('region', { name: 'Invite link created' });
      expect(posted.map((p) => p.body)).toEqual([{ expiresInHours: 48, maxUses: 3 }]);
    });

    it("leaves expiry and uses to the server for as long as the settings haven't arrived", async () => {
      const settings = gate();
      const invites = [invite()];
      serve('/api/v1/invites', () => ({ invites }));
      on('get', '/api/v1/admin/settings', async () => {
        await settings.opened;
        return HttpResponse.json(settingsResponse({ inviteDefaultTtlHours: 48, inviteDefaultMaxUses: 3 }));
      });
      const { user } = renderAdmin({ path: '/admin/invites' });
      await screen.findByRole('row', { name: /^for Sam/ });
      const expires = screen.getByLabelText('Expires after');
      const uses = screen.getByLabelText('Can be used');
      // The form doesn't know the defaults yet: it doesn't make any up.
      expect(expires).toHaveDisplayValue('Server default');
      expect(uses).toHaveDisplayValue('Server default');
      const posted = createWorks({ invites });
      await user.click(screen.getByRole('button', { name: 'Create link' }));
      await screen.findByRole('region', { name: 'Invite link created' });
      expect(posted.map((p) => p.body)).toEqual([{}]);

      settings.open();
      await waitFor(() => {
        expect(expires).toHaveDisplayValue('2 days');
      });
      expect(uses).toHaveDisplayValue('3 times');
      expect(optionsOf(expires)).not.toContain('Server default');
      expect(optionsOf(uses)).not.toContain('Server default');
    });

    it('creates the invite and shows its link once', async () => {
      const { user, state } = await openInvites({ invites: [] });
      const posted = createWorks(state);
      await user.selectOptions(screen.getByLabelText('Expires after'), '1 day');
      await user.selectOptions(screen.getByLabelText('Can be used'), '1 time');
      await user.type(screen.getByLabelText('Note'), '  for Kim ');
      await user.click(screen.getByRole('button', { name: 'Create link' }));

      const created = within(await screen.findByRole('region', { name: 'Invite link created' }));
      const link = created.getByLabelText('Invite link');
      expect(link).toHaveValue(LINK);
      expect(link).toHaveAttribute('readonly');
      expect(link).toHaveAccessibleDescription(expect.stringMatching(/Copy it now.*shown only once/));
      // Focus is on the link, ready to copy.
      expect(link).toHaveFocus();
      expect(posted.map((p) => p.body)).toEqual([{ expiresInHours: 24, maxUses: 1, note: 'for Kim' }]);
      // The list shows the new invite, and the note field is ready for the next one.
      expect(await screen.findByRole('row', { name: /^for Kim/ })).toBeInTheDocument();
      expect(screen.getByLabelText('Note')).toHaveValue('');

      await user.click(created.getByRole('button', { name: 'Copy link' }));
      await expect(navigator.clipboard.readText()).resolves.toBe(LINK);

      await user.click(created.getByRole('button', { name: 'Done' }));
      expect(screen.queryByRole('region', { name: 'Invite link created' })).not.toBeInTheDocument();
      expect(document.body).not.toHaveTextContent('EXAMPLEinviteTOKEN');
    });

    it('leaves the note out when there is none', async () => {
      const { user, state } = await openInvites();
      await defaultsShown();
      const posted = createWorks(state);
      await user.click(screen.getByRole('button', { name: 'Create link' }));
      await screen.findByRole('region', { name: 'Invite link created' });
      expect(posted.map((p) => p.body)).toEqual([{ expiresInHours: 168, maxUses: 10 }]);
    });

    it.each([
      [
        'limit_reached for the server',
        409,
        { code: 'limit_reached', params: { limit: 'invites' } },
        'This server has as many active invites as it allows. Revoke some first.',
      ],
      [
        'limit_reached for a member',
        409,
        { code: 'limit_reached', params: { limit: 'member_invites' } },
        "You have as many active invites as you're allowed. Revoke some first.",
      ],
      ['registration_closed', 403, { code: 'registration_closed' }, "This server isn't taking new accounts."],
      ['forbidden', 403, { code: 'forbidden' }, "You're not allowed to do that."],
    ] as const)('shows %s above the form', async (_name, status, error, shown) => {
      const { user } = await openInvites();
      on('post', '/api/v1/invites', () => apiError(status, error));
      await user.click(screen.getByRole('button', { name: 'Create link' }));
      expect(await screen.findByRole('alert')).toHaveTextContent(shown);
      expect(screen.queryByRole('region', { name: 'Invite link created' })).not.toBeInTheDocument();
    });

    it('shows the field codes of a 422 under their controls', async () => {
      const { user } = await openInvites();
      on('post', '/api/v1/invites', () =>
        apiError(422, {
          code: 'validation_failed',
          fields: { note: 'invalid', expiresInHours: 'out_of_range', maxUses: 'out_of_range' },
        }),
      );
      await user.click(screen.getByRole('button', { name: 'Create link' }));
      await waitFor(() => {
        expect(screen.getByLabelText('Note')).toHaveAccessibleDescription(
          expect.stringContaining("That value can't be used."),
        );
      });
      expect(screen.getByLabelText('Expires after')).toHaveAccessibleDescription(
        'That number is outside the allowed range.',
      );
      expect(screen.getByLabelText('Can be used')).toBeInvalid();
      // Focus goes to the first control that needs fixing.
      expect(screen.getByLabelText('Expires after')).toHaveFocus();
    });
  });

  describe('revoke', () => {
    it('revokes after a question', async () => {
      const { user, state, services } = await openInvites();
      const deleted = on('delete', '/api/v1/invites/:id', ({ params }) => {
        state.invites = state.invites.map((i) => (i.id === params['id'] ? { ...i, state: 'revoked' } : i));
        return noContent();
      });
      await user.click(await screen.findByRole('button', { name: 'Revoke the invite "for Sam"' }));
      const box = within(dialog('Revoke this invite?'));
      expect(box.getByText(/stops working right away/)).toBeInTheDocument();
      await user.click(box.getByRole('button', { name: 'Revoke' }));
      // Revoked invites are hidden by default.
      expect(await screen.findByText('No invite link works right now.')).toBeInTheDocument();
      expect(deleted.map((d) => d.url)).toEqual(['/api/v1/invites/h6j8k0m2n4p6']);
      expect(toasts(services)).toEqual(['The invite was revoked.']);
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      // The Revoke button is gone: focus is on the page's heading, not nowhere.
      expect(screen.getByRole('heading', { level: 1, name: 'Invites' })).toHaveFocus();
    });

    it('says why it failed and refreshes the list', async () => {
      const { user, gets } = await openInvites();
      on('delete', '/api/v1/invites/:id', () => apiError(404, { code: 'not_found' }));
      await user.click(await screen.findByRole('button', { name: 'Revoke the invite "for Sam"' }));
      const before = gets.length;
      const box = within(dialog('Revoke this invite?'));
      await user.click(box.getByRole('button', { name: 'Revoke' }));
      expect(await box.findByRole('alert')).toHaveTextContent("That doesn't exist.");
      await waitFor(() => {
        expect(gets.length).toBeGreaterThan(before);
      });
    });
  });

  it('tells an admin that links are dead while registration is closed, with the way to Settings', async () => {
    await openInvites({ registration: 'closed' });
    expect(screen.getByRole('status')).toHaveTextContent(
      "Registration is closed, so invite links don't work right now.",
    );
    expect(screen.getByRole('link', { name: 'Open Settings' })).toHaveAttribute('href', '/admin/settings');
  });

  describe('for a member who may invite', () => {
    it("shows their invites and leaves expiry and uses to the server's defaults, without asking for the settings", async () => {
      const { settingsGets, user, state } = await openInvites({ member: true });
      expect(await screen.findByRole('row', { name: /^for Sam/ })).toBeInTheDocument();
      expect(screen.getByText(/^Your invite links\./)).toBeInTheDocument();
      // A member can't read the settings, so the form can't name the defaults: the server applies them.
      const expires = screen.getByLabelText('Expires after');
      const uses = screen.getByLabelText('Can be used');
      expect(expires).toHaveDisplayValue('Server default');
      expect(uses).toHaveDisplayValue('Server default');
      expect(optionsOf(expires)).toEqual(['Server default', '1 hour', '1 day', '7 days', '30 days']);
      expect(optionsOf(uses)).toEqual(['Server default', '1 time', '10 times', '50 times']);
      const posted = createWorks(state);
      await user.type(screen.getByLabelText('Note'), 'for Kim');
      await user.click(screen.getByRole('button', { name: 'Create link' }));
      await screen.findByRole('region', { name: 'Invite link created' });
      expect(posted.map((p) => p.body)).toEqual([{ note: 'for Kim' }]);
      expect(settingsGets).toEqual([]);
    });

    it('sends what the member picked, and nothing again for a choice put back to the default', async () => {
      const { user, state } = await openInvites({ member: true });
      const expires = screen.getByLabelText('Expires after');
      const uses = screen.getByLabelText('Can be used');
      const posted = createWorks(state);
      await user.selectOptions(expires, '1 day');
      await user.selectOptions(uses, '50 times');
      // "Server default" stays on offer.
      expect(optionsOf(expires)).toEqual(['Server default', '1 hour', '1 day', '7 days', '30 days']);
      const submit = screen.getByRole('button', { name: 'Create link' });
      await user.click(submit);
      await screen.findByRole('region', { name: 'Invite link created' });
      // The form takes the next submit once this one is through.
      await waitFor(() => {
        expect(submit).not.toHaveAttribute('aria-busy');
      });
      await user.selectOptions(uses, 'Server default');
      await user.click(submit);
      await waitFor(() => {
        expect(posted).toHaveLength(2);
      });
      expect(posted.map((p) => p.body)).toEqual([{ expiresInHours: 24, maxUses: 50 }, { expiresInHours: 24 }]);
    });

    it('says that links are dead while registration is closed, without the way to Settings', async () => {
      await openInvites({ member: true, registration: 'closed' });
      expect(screen.getByRole('status')).toHaveTextContent(
        "Registration is closed, so invite links don't work right now.",
      );
      expect(screen.queryByRole('link', { name: 'Open Settings' })).not.toBeInTheDocument();
    });
  });
});
