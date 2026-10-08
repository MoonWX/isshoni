// /admin/settings (05 §15.3, 03 §9) against MSW: the fields, the ones config pins (read-only, "Set in
// isshoni.toml"), and what Save sends: only what changed, never a pinned field, never null.
import { screen, waitFor, within } from '@testing-library/react';
import { HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import type { Settings, SettingsResponse } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { apiError } from '../test/msw';
import { on, renderAdmin, serve, settingsResponse, toasts, typeInto } from './testing/harness';

/** GET and PATCH /api/v1/admin/settings over one response the test changes; PATCH merges like the server. */
function settingsApi(initial: SettingsResponse) {
  const state = { response: initial };
  const gets = serve('/api/v1/admin/settings', () => state.response);
  const patches = on('patch', '/api/v1/admin/settings', ({ body }) => {
    state.response = { ...state.response, settings: { ...state.response.settings, ...(body as Partial<Settings>) } };
    return HttpResponse.json(state.response);
  });
  return { state, gets, patches };
}

async function openSettings(initial: SettingsResponse = settingsResponse()) {
  const api = settingsApi(initial);
  const page = renderAdmin({ path: '/admin/settings' });
  await screen.findByLabelText('Server name');
  return { ...api, ...page };
}

const save = () => screen.getByRole('button', { name: 'Save changes' });

describe('SettingsPage', () => {
  it('shows every setting with its value, and no field for setupWizardDone', async () => {
    await openSettings(
      settingsResponse({
        serverName: 'Movie club',
        registrationMode: 'approval',
        inviteDefaultTtlHours: 48,
        inviteDefaultMaxUses: 5,
        membersCanInvite: true,
        maxParticipantsPerRoom: 12,
        maxSharesPerRoom: 4,
        maxShareBitrateKbps: 8000,
        transferAlertGb: 500,
        updateCheck: false,
        minClientVersion: '0.2.0',
      }),
    );
    expect(screen.getByLabelText('Server name')).toHaveValue('Movie club');
    const modes = within(screen.getByRole('group', { name: 'Who can create an account' }));
    expect(modes.getByRole('radio', { name: 'Invite, or ask an admin' })).toBeChecked();
    expect(modes.getByRole('radio', { name: 'Invite only' })).not.toBeChecked();
    expect(modes.getByRole('radio', { name: 'Closed' })).toHaveAccessibleDescription(
      expect.stringContaining('Invite links stop working'),
    );
    expect(screen.getByLabelText('Invite links expire after (hours)')).toHaveValue('48');
    expect(screen.getByLabelText('Uses per invite link')).toHaveValue('5');
    expect(screen.getByRole('checkbox', { name: 'Members can create invite links' })).toBeChecked();
    expect(screen.getByLabelText('People per room')).toHaveValue('12');
    expect(screen.getByLabelText('Shares per room')).toHaveValue('4');
    expect(screen.getByLabelText('Share bitrate cap (kbps)')).toHaveValue('8000');
    expect(screen.getByLabelText('Monthly transfer alert (GB)')).toHaveValue('500');
    expect(screen.getByLabelText('Oldest app version allowed')).toHaveValue('0.2.0');
    expect(screen.getByRole('checkbox', { name: 'Check for new isshoni releases' })).not.toBeChecked();
    // Ranges and defaults are in the hints.
    expect(screen.getByLabelText('Invite links expire after (hours)')).toHaveAccessibleDescription(
      '1 to 720. New invite links start with this. Default: 168.',
    );
    expect(screen.getByLabelText('Share bitrate cap (kbps)')).toHaveAccessibleDescription(
      '500 to 100000, or 0 to let each quality preset decide. Default: 0.',
    );
    // Eleven settings, eleven controls (the three radios are one setting).
    expect(screen.getAllByRole('textbox')).toHaveLength(8);
    expect(screen.getAllByRole('checkbox')).toHaveLength(2);
    expect(screen.getAllByRole('radio')).toHaveLength(3);
    expect(document.body).not.toHaveTextContent(/wizard/i);
    expect(save()).toBeDisabled();
  });

  describe('fields pinned by config', () => {
    const LOCKED = ['registrationMode', 'maxSharesPerRoom', 'minClientVersion', 'updateCheck'];

    it('are read-only, with "Set in isshoni.toml" and the key that sets them', async () => {
      const { user } = await openSettings(settingsResponse({ maxSharesPerRoom: 4, minClientVersion: '0.2.0' }, LOCKED));

      const shares = screen.getByLabelText('Shares per room');
      expect(shares).toHaveAttribute('readonly');
      expect(shares).toHaveAccessibleDescription(
        expect.stringMatching(/^Set in isshoni\.toml \(limits\.max_shares_per_room\)\. 0 to 1000\./),
      );
      await user.type(shares, '9');
      expect(shares).toHaveValue('4');

      const version = screen.getByLabelText('Oldest app version allowed');
      expect(version).toHaveAttribute('readonly');
      expect(version).toHaveAccessibleDescription(
        expect.stringContaining('Set in isshoni.toml (clients.min_version).'),
      );

      const check = screen.getByRole('checkbox', { name: 'Check for new isshoni releases' });
      expect(check).toBeDisabled();
      expect(check).toHaveAccessibleDescription(
        expect.stringContaining('Set in isshoni.toml (updates.release_check).'),
      );

      const modes = screen.getByRole('group', { name: 'Who can create an account' });
      expect(modes).toHaveAccessibleDescription('Set in isshoni.toml (registration.mode).');
      for (const radio of within(modes).getAllByRole('radio')) expect(radio).toBeDisabled();
      await user.click(within(modes).getByRole('radio', { name: 'Closed' }));
      expect(within(modes).getByRole('radio', { name: 'Invite only' })).toBeChecked();

      // Nothing was changed, so there is nothing to save.
      expect(save()).toBeDisabled();
      // The others stay editable.
      expect(screen.getByLabelText('People per room')).not.toHaveAttribute('readonly');
      expect(screen.getByLabelText('People per room')).not.toHaveAccessibleDescription(
        expect.stringContaining('isshoni.toml'),
      );
    });

    it('are left out of the PATCH entirely', async () => {
      const { user, patches } = await openSettings(settingsResponse({ maxSharesPerRoom: 4 }, LOCKED));
      await typeInto(user, screen.getByLabelText('People per room'), '8');
      await user.click(save());
      await waitFor(() => {
        expect(patches).toHaveLength(1);
      });
      expect(patches[0]?.body).toEqual({ maxParticipantsPerRoom: 8 });
    });

    it('says "Set in isshoni.toml" without a key for a field this build has no key for', async () => {
      await openSettings(settingsResponse({}, ['serverName']));
      const name = screen.getByLabelText('Server name');
      expect(name).toHaveAttribute('readonly');
      expect(name).toHaveAccessibleDescription(expect.stringMatching(/^Set in isshoni\.toml\. Friends see it/));
    });

    it('turns a field read-only when the server says it was pinned meanwhile (409 setting_locked)', async () => {
      const api = settingsApi(settingsResponse());
      const { user } = renderAdmin({ path: '/admin/settings' });
      const shares = await screen.findByLabelText('Shares per room');
      // The config changed and the server reloaded it after this page loaded the settings.
      const refused = on('patch', '/api/v1/admin/settings', () => {
        api.state.response = settingsResponse({ maxSharesPerRoom: 2 }, ['maxSharesPerRoom']);
        return apiError(409, { code: 'setting_locked', params: { field: 'maxSharesPerRoom' } });
      });
      await typeInto(user, shares, '6');
      await typeInto(user, screen.getByLabelText('People per room'), '8');
      await user.click(save());
      expect(await screen.findByRole('alert')).toHaveTextContent(
        "This setting is fixed in the server's config file (maxSharesPerRoom).",
      );
      expect(refused[0]?.body).toEqual({ maxParticipantsPerRoom: 8, maxSharesPerRoom: 6 });
      await waitFor(() => {
        expect(shares).toHaveAttribute('readonly');
      });
      expect(shares).toHaveValue('2');
      // The other change is still there, and saving again sends it alone.
      const saved = on('patch', '/api/v1/admin/settings', () => HttpResponse.json(api.state.response));
      await user.click(save());
      await waitFor(() => {
        expect(saved).toHaveLength(1);
      });
      expect(saved[0]?.body).toEqual({ maxParticipantsPerRoom: 8 });
    });
  });

  describe('Save', () => {
    it('sends only the fields that changed, and nothing else', async () => {
      const { user, patches, services } = await openSettings(settingsResponse({ serverName: 'Movie club' }));
      await typeInto(user, screen.getByLabelText('Server name'), 'Film club');
      await typeInto(user, screen.getByLabelText('Shares per room'), '4');
      await user.click(screen.getByRole('radio', { name: 'Invite, or ask an admin' }));
      await user.click(screen.getByRole('checkbox', { name: 'Members can create invite links' }));
      // Changed and changed back: not a change.
      await typeInto(user, screen.getByLabelText('People per room'), '5');
      await typeInto(user, screen.getByLabelText('People per room'), '0');
      await user.click(screen.getByRole('checkbox', { name: 'Check for new isshoni releases' }));
      await user.click(screen.getByRole('checkbox', { name: 'Check for new isshoni releases' }));
      expect(screen.getByText('You have unsaved changes.')).toBeInTheDocument();
      // An audit page that was looked at before is in the cache.
      const auditKey = [...queryKeys.adminAudit, { action: '', actor: '', target: '' }];
      services.queryClient.setQueryData(auditKey, { pages: [], pageParams: [] });

      await user.click(save());
      await waitFor(() => {
        expect(toasts(services)).toEqual(['Settings saved.']);
      });
      expect(patches).toHaveLength(1);
      expect(patches[0]?.body).toEqual({
        serverName: 'Film club',
        registrationMode: 'approval',
        membersCanInvite: true,
        maxSharesPerRoom: 4,
      });
      // The form shows what the server answered, and has nothing left to save.
      expect(screen.getByLabelText('Server name')).toHaveValue('Film club');
      expect(screen.getByLabelText('Shares per room')).toHaveValue('4');
      expect(save()).toBeDisabled();
      expect(screen.queryByText('You have unsaved changes.')).not.toBeInTheDocument();
      // /info reports the server name and the registration mode, and the change is in the audit log: both are
      // stale now, and asked for again by the next page that shows them.
      expect(services.queryClient.getQueryState(queryKeys.info)?.isInvalidated).toBe(true);
      expect(services.queryClient.getQueryState(auditKey)?.isInvalidated).toBe(true);
    });

    it('submits with Enter in a field', async () => {
      const { user, patches } = await openSettings();
      await typeInto(user, screen.getByLabelText('Monthly transfer alert (GB)'), '500{Enter}');
      await waitFor(() => {
        expect(patches.map((p) => p.body)).toEqual([{ transferAlertGb: 500 }]);
      });
    });

    it('resets a field by sending its default, never null', async () => {
      const { user, patches } = await openSettings(
        settingsResponse({ serverName: 'Movie club', maxSharesPerRoom: 4, minClientVersion: '0.2.0' }),
      );
      await typeInto(user, screen.getByLabelText('Server name'), '');
      await typeInto(user, screen.getByLabelText('Shares per room'), '0');
      await typeInto(user, screen.getByLabelText('Oldest app version allowed'), '');
      await user.click(save());
      await waitFor(() => {
        expect(patches).toHaveLength(1);
      });
      expect(patches[0]?.body).toEqual({ serverName: '', maxSharesPerRoom: 0, minClientVersion: '' });
    });

    it('has nothing to save once every change is undone, and Discard undoes them all', async () => {
      const { user, patches } = await openSettings();
      const name = screen.getByLabelText('Server name');
      await typeInto(user, name, 'Film club');
      expect(save()).toBeEnabled();
      await typeInto(user, name, '');
      expect(save()).toBeDisabled();

      await typeInto(user, name, 'Film club');
      await user.click(screen.getByRole('radio', { name: 'Closed' }));
      await user.click(screen.getByRole('button', { name: 'Discard changes' }));
      expect(name).toHaveValue('');
      expect(screen.getByRole('radio', { name: 'Invite only' })).toBeChecked();
      expect(save()).toBeDisabled();
      expect(screen.getByRole('button', { name: 'Discard changes' })).toBeDisabled();
      expect(patches).toEqual([]);
    });

    it('keeps a change that was made while the save was on its way', async () => {
      const { user, state, services } = await openSettings();
      let answer = (): void => undefined;
      const held = new Promise<void>((resolve) => {
        answer = resolve;
      });
      const patches = on('patch', '/api/v1/admin/settings', async ({ body }) => {
        await held;
        state.response = {
          ...state.response,
          settings: { ...state.response.settings, ...(body as Partial<Settings>) },
        };
        return HttpResponse.json(state.response);
      });
      await typeInto(user, screen.getByLabelText('Server name'), 'Film club');
      await user.click(save());
      await waitFor(() => {
        expect(patches).toHaveLength(1);
      });
      await typeInto(user, screen.getByLabelText('Shares per room'), '4');
      answer();
      await waitFor(() => {
        expect(toasts(services)).toEqual(['Settings saved.']);
      });
      // What was saved is saved; the later change is still waiting.
      expect(screen.getByLabelText('Server name')).toHaveValue('Film club');
      expect(screen.getByLabelText('Shares per room')).toHaveValue('4');
      expect(save()).toBeEnabled();
      await user.click(save());
      await waitFor(() => {
        expect(patches).toHaveLength(2);
      });
      expect(patches.map((p) => p.body)).toEqual([{ serverName: 'Film club' }, { maxSharesPerRoom: 4 }]);
    });

    it('holds back values that can not be sent, says why under each, and sends nothing', async () => {
      const { user, patches } = await openSettings();
      const hours = screen.getByLabelText('Invite links expire after (hours)');
      const uses = screen.getByLabelText('Uses per invite link');
      const bitrate = screen.getByLabelText('Share bitrate cap (kbps)');
      const version = screen.getByLabelText('Oldest app version allowed');
      await typeInto(user, hours, '');
      await typeInto(user, uses, 'ten');
      await typeInto(user, bitrate, '100');
      await typeInto(user, version, 'v2');
      await user.click(save());

      expect(hours).toHaveAccessibleDescription(expect.stringContaining("This can't be empty."));
      expect(uses).toHaveAccessibleDescription(expect.stringContaining("That value can't be used."));
      expect(bitrate).toHaveAccessibleDescription(expect.stringContaining('That number is outside the allowed range.'));
      expect(version).toHaveAccessibleDescription(expect.stringContaining("That value can't be used."));
      for (const field of [hours, uses, bitrate, version]) expect(field).toBeInvalid();
      // Focus goes to the first field that needs fixing.
      expect(hours).toHaveFocus();
      expect(patches).toEqual([]);

      // Fixing a field takes its message away; the others keep theirs.
      await typeInto(user, hours, '24');
      expect(hours).toBeValid();
      expect(uses).toBeInvalid();
    });

    it('shows the field codes of a 422 under their controls', async () => {
      const { user } = await openSettings();
      on('patch', '/api/v1/admin/settings', () =>
        apiError(422, {
          code: 'validation_failed',
          fields: { serverName: 'invalid', maxParticipantsPerRoom: 'out_of_range', registrationMode: 'invalid' },
        }),
      );
      const name = screen.getByLabelText('Server name');
      const people = screen.getByLabelText('People per room');
      await typeInto(user, name, 'Film club');
      await typeInto(user, people, '8');
      await user.click(screen.getByRole('radio', { name: 'Closed' }));
      await user.click(save());
      await waitFor(() => {
        // serverName has a text of its own (fieldErrors.serverName.invalid).
        expect(name).toHaveAccessibleDescription(
          expect.stringContaining("That name has characters that can't be used."),
        );
      });
      expect(people).toHaveAccessibleDescription(expect.stringContaining('That number is outside the allowed range.'));
      expect(screen.getByRole('group', { name: 'Who can create an account' })).toHaveAccessibleDescription(
        "That value can't be used.",
      );
      // What was typed stays.
      expect(name).toHaveValue('Film club');
      expect(save()).toBeEnabled();
    });

    it.each([
      ['forbidden', 403, { code: 'forbidden' }, "You're not allowed to do that."],
      [
        'internal',
        500,
        { code: 'internal', requestId: 'req-3' },
        'Something went wrong on the server. Reference: req-3',
      ],
    ] as const)('shows %s beside the button and keeps the changes', async (_name, status, error, shown) => {
      const { user } = await openSettings();
      on('patch', '/api/v1/admin/settings', () => apiError(status, error));
      await typeInto(user, screen.getByLabelText('Server name'), 'Film club');
      await user.click(save());
      expect(await screen.findByRole('alert')).toHaveTextContent(shown);
      expect(screen.getByLabelText('Server name')).toHaveValue('Film club');
      expect(save()).toBeEnabled();
    });
  });

  it('follows the server for fields that were not touched, and keeps the ones that were', async () => {
    const { user, state, services } = await openSettings(settingsResponse({ maxSharesPerRoom: 4 }));
    await typeInto(user, screen.getByLabelText('Server name'), 'Film club');
    // Another admin changed a setting; this page hears about it and asks again.
    state.response = settingsResponse({ maxSharesPerRoom: 6, transferAlertGb: 100 });
    await services.queryClient.invalidateQueries({ queryKey: queryKeys.adminSettings });
    await waitFor(() => {
      expect(screen.getByLabelText('Shares per room')).toHaveValue('6');
    });
    expect(screen.getByLabelText('Monthly transfer alert (GB)')).toHaveValue('100');
    expect(screen.getByLabelText('Server name')).toHaveValue('Film club');
  });

  it('says why the settings could not load', async () => {
    on('get', '/api/v1/admin/settings', () => apiError(503, { code: 'server_shutdown' }));
    renderAdmin({ path: '/admin/settings' });
    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't load this. The server is restarting.");
    expect(screen.queryByRole('button', { name: 'Save changes' })).not.toBeInTheDocument();
  });
});
