// /admin/audit (05 §15.3, 03 §10, §12.4.8) against MSW: how rows read, paging with "Load more" by nextBefore, and
// the three filters.
import { screen, waitFor, within } from '@testing-library/react';
import { HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import type { AuditEntry, AuditPage } from '../protocol/api.gen';
import { apiError } from '../test/msw';
import { auditPath, AUDIT_PAGE_SIZE } from './adminApi';
import { actionLabel, detailLines } from './auditText';
import { adminUser, auditEntry, on, renderAdmin, rowOf, selfUser, serve, type Sent } from './testing/harness';
import { i18n } from '../i18n';

const ADMIN = 'a1b2c3d4e5f6';
const SAM = 'b8f2n4r6t0vz';

/** Rows with descending ids from `from`, like the log: newest first. */
function rows(from: number, count: number, overrides: Partial<AuditEntry> = {}): AuditEntry[] {
  return Array.from({ length: count }, (_, i) =>
    auditEntry({
      id: from - i,
      action: 'auth.login',
      target: { kind: 'session', id: `s${String(from - i)}` },
      detail: {},
      ...overrides,
    }),
  );
}

/** GET /api/v1/admin/audit answered from the query, and the users the filters offer. */
function auditApi(respond: (query: URLSearchParams, sent: Sent) => AuditPage | Response) {
  serve('/api/v1/admin/users', () => ({ users: [selfUser(), adminUser()] }));
  return on('get', '/api/v1/admin/audit', (sent) => {
    const answer = respond(new URL(sent.url, 'http://x').searchParams, sent);
    return answer instanceof Response ? answer : HttpResponse.json(answer);
  });
}

async function openAudit(path = '/admin/audit') {
  const page = renderAdmin({ path });
  await screen.findByRole('heading', { level: 1, name: 'Audit log' });
  return page;
}

describe('AuditPage', () => {
  it('reads each row: what happened, when, who did it, to whom, from where, and the details', async () => {
    const gets = auditApi(() => ({
      entries: [
        auditEntry(),
        auditEntry({
          id: 4810,
          action: 'auth.login_failed',
          outcome: 'denied',
          actor: { kind: 'anonymous' },
          target: { kind: 'user', id: SAM, name: 'sam' },
          detail: { reason: 'wrong_password' },
        }),
        auditEntry({
          id: 4809,
          action: 'settings.changed',
          target: { kind: 'settings' },
          detail: {
            changes: {
              registrationMode: { from: 'invite', to: 'approval' },
              serverName: { from: '', to: 'Film club' },
            },
          },
        }),
        auditEntry({
          id: 4808,
          action: 'invite.created',
          target: { kind: 'invite', id: 'h6j8k0m2n4p6' },
          detail: { maxUses: 10, expiresAt: '2026-10-08T12:00:00.000Z', note: 'for Sam' },
        }),
        auditEntry({
          id: 4807,
          action: 'setup.token_issued',
          actor: { kind: 'cli' },
          target: undefined,
          ip: undefined,
          detail: {},
        }),
        auditEntry({
          id: 4806,
          action: 'secrets.rotated',
          actor: { kind: 'system' },
          target: undefined,
          detail: { keys: ['session', 'invite'] },
        }),
        auditEntry({
          id: 4805,
          action: 'user.signup_requested',
          actor: { kind: 'anonymous', name: 'sam_k' },
          target: { kind: 'user', id: 'p1p2p3p4p5p6', name: 'sam_k' },
          ip: '198.51.100.23',
          detail: {},
        }),
        auditEntry({
          id: 4804,
          action: 'room.renamed',
          target: { kind: 'room', id: 'p4t7w2m9k1qs', name: 'Games' },
          detail: { from: 'Movie night', to: 'Games' },
        }),
        // An action of a newer server: shown as it is.
        auditEntry({
          id: 4803,
          action: 'device.teleported',
          target: { kind: 'gadget', id: 'g1' },
          detail: { nested: { a: 1 } },
        }),
      ],
      nextBefore: null,
    }));
    await openAudit();
    await screen.findByRole('table', { name: 'Audit log' });
    expect(gets[0]?.url).toBe(`/api/v1/admin/audit?limit=${String(AUDIT_PAGE_SIZE)}`);
    expect(screen.getAllByRole('columnheader').map((th) => th.textContent)).toEqual([
      'Action',
      'When',
      'By',
      'To',
      'IP address',
      'Details',
    ]);

    const role = within(rowOf(/^Changed a role/));
    expect(role.getByRole('cell', { name: /2026/ }).querySelector('time')).toHaveAttribute(
      'datetime',
      '2026-10-03T19:22:05.114Z',
    );
    expect(role.getByRole('button', { name: 'admin' })).toBeInTheDocument();
    expect(role.getByRole('button', { name: 'sam' })).toBeInTheDocument();
    expect(role.getByText('203.0.113.7')).toBeInTheDocument();
    expect(role.getByText('user → admin')).toBeInTheDocument();
    expect(role.queryByText('Denied')).not.toBeInTheDocument();

    const failed = within(rowOf(/^Failed to log in/));
    expect(failed.getByText('Denied')).toBeInTheDocument();
    expect(failed.getByText('Not signed in')).toBeInTheDocument();
    expect(failed.getByText('reason: wrong_password')).toBeInTheDocument();

    const settings = within(rowOf(/^Changed settings/));
    expect(settings.getByText('Settings')).toBeInTheDocument();
    expect(settings.getByText('registrationMode: invite → approval')).toBeInTheDocument();
    expect(settings.getByText('serverName: "" → Film club')).toBeInTheDocument();

    const invited = within(rowOf(/^Created an invite/));
    expect(invited.getByRole('button', { name: 'h6j8k0m2n4p6' })).toBeInTheDocument();
    expect(invited.getByText('Invite')).toBeInTheDocument();
    expect(invited.getByText('maxUses: 10')).toBeInTheDocument();
    expect(invited.getByText('note: for Sam')).toBeInTheDocument();

    expect(within(rowOf(/^Created a setup link/)).getByText('Server command line')).toBeInTheDocument();
    const rotated = within(rowOf(/^Rotated the server's keys/));
    expect(rotated.getByText('The server')).toBeInTheDocument();
    expect(rotated.getByText('keys: session, invite')).toBeInTheDocument();
    expect(within(rowOf(/^Asked for an account/)).getByText('sam_k (not signed in)')).toBeInTheDocument();
    const renamed = within(rowOf(/^Renamed a room/));
    expect(renamed.getByText('Room')).toBeInTheDocument();
    expect(renamed.getByText('Movie night → Games')).toBeInTheDocument();

    const unknown = within(rowOf(/^device\.teleported/));
    expect(unknown.getByText('gadget')).toBeInTheDocument();
    expect(unknown.getByText('nested: {"a":1}')).toBeInTheDocument();
    // The whole log fits one page: nothing more to load.
    expect(screen.queryByRole('button', { name: 'Load more' })).not.toBeInTheDocument();
    expect(screen.getByText('No older entries.')).toBeInTheDocument();
  });

  describe('paging', () => {
    it('loads the next page with the previous nextBefore, until the server says null', async () => {
      // Short pages keep the test fast; the cursor is what matters, and the server may send fewer rows than asked.
      const gets = auditApi((query) => {
        const before = query.get('before');
        if (before === null) return { entries: rows(300, 4), nextBefore: 297 };
        if (before === '297') return { entries: rows(296, 4), nextBefore: 293 };
        if (before === '293') return { entries: rows(292, 2), nextBefore: null };
        return apiError(400, { code: 'bad_request' });
      });
      const { user, services } = await openAudit();
      await screen.findByRole('table', { name: 'Audit log' });
      expect(screen.getAllByRole('row')).toHaveLength(1 + 4);

      await user.click(screen.getByRole('button', { name: 'Load more' }));
      await waitFor(() => {
        expect(screen.getAllByRole('row')).toHaveLength(1 + 8);
      });
      expect(gets.map((g) => g.url)).toEqual([
        '/api/v1/admin/audit?limit=50',
        '/api/v1/admin/audit?limit=50&before=297',
      ]);
      await waitFor(() => {
        expect(services.ui.getState().announcements.polite?.text).toBe('Showing 8 entries.');
      });
      // More to come: the button is still there, and still has the focus.
      expect(screen.getByRole('button', { name: 'Load more' })).toHaveFocus();

      await user.click(screen.getByRole('button', { name: 'Load more' }));
      await waitFor(() => {
        expect(screen.getAllByRole('row')).toHaveLength(1 + 10);
      });
      expect(gets.at(-1)?.url).toBe('/api/v1/admin/audit?limit=50&before=293');
      // The last page: no button, and focus on the line that says so.
      expect(screen.queryByRole('button', { name: 'Load more' })).not.toBeInTheDocument();
      expect(screen.getByText('No older entries.')).toHaveFocus();
      // Rows keep the log's order, newest first, with none twice.
      const ids = services.queryClient
        .getQueryData<{ pages: AuditPage[] }>(['admin', 'audit', { action: '', actor: '', target: '' }])
        ?.pages.flatMap((p) => p.entries.map((e) => e.id));
      expect(ids).toEqual([300, 299, 298, 297, 296, 295, 294, 293, 292, 291]);
    });

    it('keeps what is loaded when the next page fails, and tries again', async () => {
      let fail = true;
      auditApi((query) => {
        if (query.get('before') === null) return { entries: rows(10, 3), nextBefore: 8 };
        return fail ? apiError(500, { code: 'internal' }) : { entries: rows(7, 3), nextBefore: null };
      });
      const { user } = await openAudit();
      await screen.findByRole('table', { name: 'Audit log' });
      await user.click(screen.getByRole('button', { name: 'Load more' }));
      expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't load more. Try again.");
      expect(screen.getAllByRole('row')).toHaveLength(1 + 3);
      fail = false;
      await user.click(screen.getByRole('button', { name: 'Load more' }));
      await waitFor(() => {
        expect(screen.getAllByRole('row')).toHaveLength(1 + 6);
      });
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    });
  });

  describe('filters', () => {
    it('asks the server again with the action group, the actor and the target', async () => {
      const gets = auditApi(() => ({ entries: [auditEntry()], nextBefore: null }));
      const { user, router } = await openAudit();
      await screen.findByRole('table', { name: 'Audit log' });
      const filters = within(screen.getByRole('form', { name: 'Filter the log' }));
      expect(
        within(filters.getByLabelText('Action'))
          .getAllByRole('option')
          .map((o) => o.textContent),
      ).toEqual([
        'Everything',
        'Logins',
        'Accounts',
        'Invites',
        'Rooms',
        'Settings',
        'Browser sessions',
        'Setup',
        'Server keys',
      ]);
      // The users of the server, by name.
      await waitFor(() => {
        expect(
          within(filters.getByLabelText('Done by'))
            .getAllByRole('option')
            .map((o) => o.textContent),
        ).toEqual(['Anyone', 'admin', 'sam']);
      });

      await user.selectOptions(filters.getByLabelText('Action'), 'Accounts');
      await waitFor(() => {
        expect(gets.at(-1)?.url).toBe('/api/v1/admin/audit?limit=50&action=user.');
      });
      await user.selectOptions(filters.getByLabelText('Done by'), 'admin');
      await waitFor(() => {
        expect(gets.at(-1)?.url).toBe(`/api/v1/admin/audit?limit=50&action=user.&actor=${ADMIN}`);
      });
      await user.selectOptions(filters.getByLabelText('Done to'), 'sam');
      await waitFor(() => {
        expect(gets.at(-1)?.url).toBe(`/api/v1/admin/audit?limit=50&action=user.&actor=${ADMIN}&target=${SAM}`);
      });
      // The filters are in the address.
      expect(router.state.location.search).toBe(`?action=user.&actor=${ADMIN}&target=${SAM}`);

      await user.click(screen.getByRole('button', { name: 'Clear filters' }));
      await waitFor(() => {
        expect(router.state.location.search).toBe('');
      });
      expect(filters.getByLabelText('Action')).toHaveValue('');
      expect(screen.queryByRole('button', { name: 'Clear filters' })).not.toBeInTheDocument();
    });

    it('starts with the filters of the address', async () => {
      const gets = auditApi(() => ({ entries: [], nextBefore: null }));
      await openAudit(`/admin/audit?action=auth.login_failed&target=${SAM}`);
      expect(await screen.findByText('Nothing in the log matches these filters.')).toBeInTheDocument();
      expect(gets[0]?.url).toBe(`/api/v1/admin/audit?limit=50&action=auth.login_failed&target=${SAM}`);
      // A single action isn't one of the groups: it is offered by its own name.
      const action = screen.getByLabelText('Action');
      expect(action).toHaveValue('auth.login_failed');
      expect(within(action).getByRole('option', { name: 'Failed to log in' })).toBeInTheDocument();
      await waitFor(() => {
        expect(screen.getByLabelText('Done to')).toHaveValue(SAM);
      });
    });

    it('filters by a name in a row, also one that is not a user anymore', async () => {
      const GONE = 'x9x9x9x9x9x9';
      const gets = auditApi((query) => ({
        entries: [
          auditEntry({
            id: 1,
            action: 'room.deleted',
            actor: { kind: 'user', id: GONE, name: 'ghost' },
            target: { kind: 'room', id: 'r1r1r1r1r1r1', name: 'Games' },
            detail: { name: 'Games' },
          }),
          ...(query.has('actor') ? [] : [auditEntry()]),
        ],
        nextBefore: null,
      }));
      const { user } = await openAudit();
      await screen.findByRole('table', { name: 'Audit log' });
      const ghost = within(rowOf(/^Deleted a room/)).getByRole('button', { name: 'ghost' });
      expect(ghost).toHaveAttribute('title', 'Show only what ghost did');
      await user.click(ghost);
      await waitFor(() => {
        expect(gets.at(-1)?.url).toBe(`/api/v1/admin/audit?limit=50&actor=${GONE}`);
      });
      // The filter names the deleted user as the log does.
      await waitFor(() => {
        expect(screen.getByLabelText('Done by')).toHaveValue(GONE);
      });
      expect(within(screen.getByLabelText('Done by')).getByRole('option', { name: 'ghost' })).toBeInTheDocument();
      // The column the log is filtered by reads as plain text.
      await waitFor(() => {
        expect(within(rowOf(/^Deleted a room/)).queryByRole('button', { name: 'ghost' })).not.toBeInTheDocument();
      });

      await user.click(within(rowOf(/^Deleted a room/)).getByRole('button', { name: 'Games' }));
      await waitFor(() => {
        expect(gets.at(-1)?.url).toBe(`/api/v1/admin/audit?limit=50&actor=${GONE}&target=r1r1r1r1r1r1`);
      });
      await waitFor(() => {
        expect(within(screen.getByLabelText('Done to')).getByRole('option', { name: 'Games' })).toBeInTheDocument();
      });
    });

    it('ignores a filter in the address that is too long to be one', async () => {
      const gets = auditApi(() => ({ entries: [], nextBefore: null }));
      await openAudit(`/admin/audit?actor=${'a'.repeat(65)}`);
      expect(await screen.findByText('Nothing in the log yet.')).toBeInTheDocument();
      expect(gets[0]?.url).toBe('/api/v1/admin/audit?limit=50');
    });
  });

  it('says why the log could not load, and tries again', async () => {
    let fail = true;
    auditApi(() => (fail ? apiError(400, { code: 'bad_request' }) : { entries: [auditEntry()], nextBefore: null }));
    const { user } = await openAudit();
    expect(await screen.findByRole('alert')).toHaveTextContent(
      "Couldn't load this. The server couldn't accept that request.",
    );
    fail = false;
    await user.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('row', { name: /^Changed a role/ })).toBeInTheDocument();
  });
});

describe('auditPath', () => {
  it('builds the query of 03 §12.4.8, leaving out what is not set', () => {
    const none = { action: '', actor: '', target: '' };
    expect(auditPath(none, null)).toBe('/api/v1/admin/audit?limit=50');
    expect(auditPath(none, 4812)).toBe('/api/v1/admin/audit?limit=50&before=4812');
    expect(auditPath({ action: 'user.', actor: ADMIN, target: SAM }, 4812)).toBe(
      `/api/v1/admin/audit?limit=50&before=4812&action=user.&actor=${ADMIN}&target=${SAM}`,
    );
    // Values are encoded: they come from the address bar.
    expect(auditPath({ action: 'a&b=c', actor: '', target: 'x y' }, null)).toBe(
      '/api/v1/admin/audit?limit=50&action=a%26b%3Dc&target=x+y',
    );
  });
});

describe('audit texts', () => {
  it('has a text for every action of 03 §10', () => {
    const actions = [
      'setup.token_issued',
      'setup.completed',
      'auth.login',
      'auth.login_failed',
      'auth.throttled',
      'auth.logout',
      'auth.logout_everywhere',
      'auth.password_changed',
      'session.revoked',
      'user.registered',
      'user.signup_requested',
      'user.approved',
      'user.signup_rejected',
      'user.signup_expired',
      'user.renamed',
      'user.role_changed',
      'user.disabled',
      'user.enabled',
      'user.deleted',
      'user.signed_out',
      'user.password_reset_issued',
      'user.password_reset_completed',
      'invite.created',
      'invite.revoked',
      'room.created',
      'room.renamed',
      'room.deleted',
      'settings.changed',
      'secrets.rotated',
      'device.linked',
      'device.revoked',
      'device.refresh_reused',
    ];
    for (const action of actions) expect(actionLabel(action, i18n.t), action).not.toBe(action);
  });

  it('shows an action without a text, or that is no action at all, as it is', () => {
    expect(actionLabel('user.teleported', i18n.t)).toBe('user.teleported');
    // A group of actions is not a message.
    expect(actionLabel('user', i18n.t)).toBe('user');
    expect(actionLabel('user.', i18n.t)).toBe('user.');
    expect(actionLabel('', i18n.t)).toBe('');
    expect(actionLabel('../title', i18n.t)).toBe('../title');
  });

  it('reads a detail object as lines', () => {
    expect(detailLines({})).toEqual([]);
    expect(detailLines(null)).toEqual([]);
    expect(detailLines('x')).toEqual([]);
    expect(detailLines({ from: 'user', to: 'admin' })).toEqual(['user → admin']);
    expect(detailLines({ sessions: 2, devices: 0 })).toEqual(['sessions: 2', 'devices: 0']);
    expect(detailLines({ self: true })).toEqual(['self: true']);
    expect(detailLines({ all: true, count: 37 })).toEqual(['all: true', 'count: 37']);
    expect(detailLines({ changes: { maxSharesPerRoom: { from: 0, to: 4 } }, truncated: true })).toEqual([
      'maxSharesPerRoom: 0 → 4',
      'truncated: true',
    ]);
    expect(detailLines({ changes: { odd: 'value' } })).toEqual(['odd: value']);
    expect(detailLines({ nothing: null, list: [1, 'a'] })).toEqual(['nothing: –', 'list: 1, a']);
  });
});
