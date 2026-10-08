// /admin/approvals (05 §15.3, 03 §7.9) against MSW: the queue, Approve and Reject per row, and "Reject all" with
// its question, its request (`all` in the path and {"all": true} in the body) and its count.
import { screen, waitFor, within } from '@testing-library/react';
import { HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import type { PendingUser } from '../protocol/api.gen';
import { apiError } from '../test/msw';
import { dialog, noContent, on, pendingUser, renderAdmin, rowOf, serve, toasts } from './testing/harness';

const KIM = pendingUser({ id: 'k1k1k1k1k1k1', username: 'kim', ip: '203.0.113.9' });
const BO = pendingUser({ id: 'b0b0b0b0b0b0', username: 'bo', ip: '203.0.113.10' });

function approvalsApi(initial: PendingUser[]) {
  const state = { pending: initial };
  const gets = serve('/api/v1/admin/approvals', () => ({ pending: state.pending }));
  return { state, gets };
}

async function openApprovals(
  initial: PendingUser[] = [pendingUser(), KIM, BO],
  registration: 'approval' | 'invite' = 'approval',
) {
  const api = approvalsApi(initial);
  const page = renderAdmin({ path: '/admin/approvals', info: { registration } });
  await screen.findByRole('heading', { level: 1, name: 'Approvals' });
  if (initial.length > 0) await screen.findByRole('table', { name: 'Approvals' });
  return { ...api, ...page };
}

describe('ApprovalsPage', () => {
  it('lists the pending sign-ups with when they asked and from which address', async () => {
    await openApprovals();
    expect(screen.getAllByRole('columnheader').map((th) => th.textContent)).toEqual([
      'Username',
      'Asked',
      'IP address',
    ]);
    const row = within(rowOf(/^sam_k/));
    expect(row.getByText('198.51.100.23')).toBeInTheDocument();
    expect(row.getByRole('cell', { name: /2026/ }).querySelector('time')).toHaveAttribute(
      'datetime',
      '2026-09-30T12:00:00.000Z',
    );
    expect(row.getByRole('button', { name: 'Approve sam_k' })).toHaveTextContent('Approve');
    expect(row.getByRole('button', { name: 'Reject sam_k' })).toHaveTextContent('Reject');
  });

  it('approves one sign-up: the row goes, and focus moves to the next one', async () => {
    const { user, state, services } = await openApprovals();
    const approved = on('post', '/api/v1/admin/approvals/:id/approve', ({ params }) => {
      state.pending = state.pending.filter((p) => p.id !== params['id']);
      return HttpResponse.json({ user: { id: params['id'], username: 'sam_k', role: 'user', status: 'active' } });
    });
    await user.click(screen.getByRole('button', { name: 'Approve sam_k' }));
    await waitFor(() => {
      expect(screen.queryByRole('row', { name: /^sam_k/ })).not.toBeInTheDocument();
    });
    expect(approved).toEqual([
      expect.objectContaining({ url: '/api/v1/admin/approvals/p1p2p3p4p5p6/approve', body: {} }),
    ]);
    expect(toasts(services)).toEqual(['sam_k can log in now.']);
    // kim moved up into the place of the row that left.
    expect(screen.getByRole('button', { name: 'Approve kim' })).toHaveFocus();
  });

  it('rejects one sign-up', async () => {
    const { user, state, services } = await openApprovals();
    const rejected = on('post', '/api/v1/admin/approvals/:id/reject', ({ params }) => {
      state.pending = state.pending.filter((p) => p.id !== params['id']);
      return noContent();
    });
    await user.click(screen.getByRole('button', { name: 'Reject bo' }));
    await waitFor(() => {
      expect(screen.queryByRole('row', { name: /^bo/ })).not.toBeInTheDocument();
    });
    expect(rejected).toEqual([
      expect.objectContaining({ url: '/api/v1/admin/approvals/b0b0b0b0b0b0/reject', body: {} }),
    ]);
    expect(toasts(services)).toEqual(["bo's request was rejected."]);
    // The last row left: the one that is last now gets the focus, on the same button.
    expect(screen.getByRole('button', { name: 'Reject kim' })).toHaveFocus();
  });

  it('works through the queue from the keyboard without ever landing on Approve', async () => {
    const { user, state } = await openApprovals();
    const rejected = on('post', '/api/v1/admin/approvals/:id/reject', ({ params }) => {
      state.pending = state.pending.filter((p) => p.id !== params['id']);
      return noContent();
    });
    const approved = on('post', '/api/v1/admin/approvals/:id/approve', () => HttpResponse.json({ user: {} }));
    screen.getByRole('button', { name: 'Reject sam_k' }).focus();
    await user.keyboard('{Enter}');
    await waitFor(() => {
      expect(screen.queryByRole('row', { name: /^sam_k/ })).not.toBeInTheDocument();
    });
    // kim moved up; Enter again answers kim the same way.
    expect(screen.getByRole('button', { name: 'Reject kim' })).toHaveFocus();
    await user.keyboard('{Enter}');
    await waitFor(() => {
      expect(screen.queryByRole('row', { name: /^kim/ })).not.toBeInTheDocument();
    });
    expect(screen.getByRole('button', { name: 'Reject bo' })).toHaveFocus();
    expect(rejected.map((r) => r.url)).toEqual([
      '/api/v1/admin/approvals/p1p2p3p4p5p6/reject',
      '/api/v1/admin/approvals/k1k1k1k1k1k1/reject',
    ]);
    expect(approved).toEqual([]);
  });

  it('moves focus to the heading when the last sign-up is answered', async () => {
    const { user, state } = await openApprovals([KIM]);
    on('post', '/api/v1/admin/approvals/:id/approve', () => {
      state.pending = [];
      return HttpResponse.json({ user: {} });
    });
    await user.click(screen.getByRole('button', { name: 'Approve kim' }));
    expect(await screen.findByText('No sign-ups are waiting.')).toBeInTheDocument();
    expect(screen.getByRole('heading', { level: 1, name: 'Approvals' })).toHaveFocus();
    expect(screen.queryByRole('button', { name: 'Reject all' })).not.toBeInTheDocument();
  });

  it('refreshes the queue when another admin answered first', async () => {
    const { user, state, services } = await openApprovals();
    on('post', '/api/v1/admin/approvals/:id/approve', () => {
      state.pending = [KIM, BO];
      return apiError(404, { code: 'user_not_found' });
    });
    await user.click(screen.getByRole('button', { name: 'Approve sam_k' }));
    await waitFor(() => {
      expect(screen.queryByRole('row', { name: /^sam_k/ })).not.toBeInTheDocument();
    });
    expect(toasts(services)).toEqual(["That user doesn't exist anymore."]);
    expect(rowOf(/^kim/)).toBeInTheDocument();
  });

  describe('Reject all', () => {
    it('asks first, sends {"all": true} to …/all/reject, and says how many it rejected', async () => {
      const { user, state } = await openApprovals();
      const rejected = on('post', '/api/v1/admin/approvals/:id/reject', () => {
        // The server rejects whatever is pending when the request arrives: more than this page listed.
        state.pending = [];
        return HttpResponse.json({ rejected: 37 });
      });
      await user.click(screen.getByRole('button', { name: 'Reject all' }));
      const box = within(dialog('Reject all pending sign-ups?'));
      expect(box.getByText('Anyone who is real can sign up again.')).toBeInTheDocument();
      // Nothing is sent before the question is answered.
      expect(rejected).toEqual([]);
      await user.click(box.getByRole('button', { name: 'Reject all' }));

      expect(await screen.findByRole('status')).toHaveTextContent('Rejected 37 sign-ups');
      expect(rejected).toEqual([
        expect.objectContaining({ url: '/api/v1/admin/approvals/all/reject', body: { all: true } }),
      ]);
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(screen.queryByRole('table')).not.toBeInTheDocument();
      expect(screen.getByText('No sign-ups are waiting.')).toBeInTheDocument();
      expect(screen.getByRole('heading', { level: 1, name: 'Approvals' })).toHaveFocus();
    });

    it('counts one sign-up in the singular', async () => {
      const { user, state } = await openApprovals([KIM]);
      on('post', '/api/v1/admin/approvals/:id/reject', () => {
        state.pending = [];
        return HttpResponse.json({ rejected: 1 });
      });
      await user.click(screen.getByRole('button', { name: 'Reject all' }));
      await user.click(within(dialog('Reject all pending sign-ups?')).getByRole('button', { name: 'Reject all' }));
      expect(await screen.findByRole('status')).toHaveTextContent(/^Rejected 1 sign-up$/);
    });

    it('rejects nothing when the question is cancelled', async () => {
      const { user } = await openApprovals();
      const rejected = on('post', '/api/v1/admin/approvals/:id/reject', () => HttpResponse.json({ rejected: 3 }));
      await user.click(screen.getByRole('button', { name: 'Reject all' }));
      await user.click(within(dialog('Reject all pending sign-ups?')).getByRole('button', { name: 'Cancel' }));
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(rejected).toEqual([]);
      expect(screen.getAllByRole('row')).toHaveLength(4);
    });

    it('keeps the queue and says why when the request fails', async () => {
      const { user } = await openApprovals();
      on('post', '/api/v1/admin/approvals/:id/reject', () => apiError(429, { code: 'rate_limited', retryAfter: 30 }));
      await user.click(screen.getByRole('button', { name: 'Reject all' }));
      const box = within(dialog('Reject all pending sign-ups?'));
      await user.click(box.getByRole('button', { name: 'Reject all' }));
      expect(await box.findByRole('alert')).toHaveTextContent('Too many attempts. Try again in 30 seconds.');
      expect(rowOf(/^sam_k/)).toBeInTheDocument();
    });

    it('forgets the last count once another sign-up is answered', async () => {
      const { user, state } = await openApprovals([KIM]);
      on('post', '/api/v1/admin/approvals/:id/reject', ({ params }) => {
        if (params['id'] === 'all') {
          // A new sign-up arrives right after the queue was emptied.
          state.pending = [BO];
          return HttpResponse.json({ rejected: 1 });
        }
        state.pending = [];
        return noContent();
      });
      await user.click(screen.getByRole('button', { name: 'Reject all' }));
      await user.click(within(dialog('Reject all pending sign-ups?')).getByRole('button', { name: 'Reject all' }));
      expect(await screen.findByRole('status')).toHaveTextContent('Rejected 1 sign-up');
      await user.click(await screen.findByRole('button', { name: 'Reject bo' }));
      await waitFor(() => {
        expect(screen.queryByRole('status')).not.toBeInTheDocument();
      });
    });
  });

  it('says that nobody waits', async () => {
    await openApprovals([]);
    expect(await screen.findByText('No sign-ups are waiting.')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Reject all' })).not.toBeInTheDocument();
  });

  it('points to Settings when sign-up requests are off', async () => {
    await openApprovals([], 'invite');
    expect(
      await screen.findByText(/People can only ask for an account while registration is set to/),
    ).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Open Settings' })).toHaveAttribute('href', '/admin/settings');
  });
});
