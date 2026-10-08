// The admin layout and the folder's contract with the router (05 §5): which pages exist, who gets the navigation,
// and the approvals badge.
import { screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { meFixture } from '../test/msw';
import * as folder from './index';
import { invite, renderAdmin, ROOM_HEADING, selfUser, serve } from './testing/harness';

describe('the admin folder', () => {
  it('exports the layout and the six pages of this slice by the names the router loads', () => {
    expect(Object.keys(folder).sort()).toEqual([
      'AdminLayout',
      'ApprovalsPage',
      'AuditPage',
      'InvitesPage',
      'RoomsPage',
      'SettingsPage',
      'UsersPage',
    ]);
  });
});

describe('AdminLayout', () => {
  it('shows an admin every admin page in the navigation, with the current one marked', async () => {
    serve('/api/v1/admin/users', () => ({ users: [selfUser()] }));
    renderAdmin({ path: '/admin/users' });
    expect(await screen.findByRole('heading', { level: 1, name: 'Users' })).toBeInTheDocument();
    const nav = screen.getByRole('navigation', { name: 'Admin' });
    const links = within(nav).getAllByRole('link');
    expect(links.map((a) => [a.textContent, a.getAttribute('href')])).toEqual([
      ['Dashboard', '/admin'],
      ['Users', '/admin/users'],
      ['Approvals', '/admin/approvals'],
      ['Invites', '/admin/invites'],
      ['Rooms', '/admin/rooms'],
      ['Settings', '/admin/settings'],
      ['Audit log', '/admin/audit'],
      ['Doctor', '/admin/doctor'],
    ]);
    expect(within(nav).getByRole('link', { name: 'Users' })).toHaveAttribute('aria-current', 'page');
    expect(within(nav).getByRole('link', { name: 'Dashboard' })).not.toHaveAttribute('aria-current');
  });

  it('counts the waiting sign-ups beside Approvals, from me.badges', async () => {
    serve('/api/v1/admin/users', () => ({ users: [selfUser()] }));
    const me = { ...meFixture({ admin: true }), badges: { pendingApprovals: 3 } };
    renderAdmin({ path: '/admin/users', me });
    expect(await screen.findByRole('link', { name: 'Approvals, 3 waiting' })).toBeInTheDocument();
  });

  it('leads back to the room', async () => {
    serve('/api/v1/admin/users', () => ({ users: [selfUser()] }));
    const { user } = renderAdmin({ path: '/admin/users' });
    await user.click(await screen.findByRole('link', { name: 'Back to isshoni' }));
    expect(await screen.findByRole('heading', { name: ROOM_HEADING })).toBeInTheDocument();
  });

  it('gives a member who may invite the Invites page without the admin navigation', async () => {
    serve('/api/v1/invites', () => ({ invites: [invite({ note: 'for Kim' })] }));
    renderAdmin({ path: '/admin/invites', me: meFixture({ createInvites: true }) });
    expect(await screen.findByRole('heading', { level: 1, name: 'Invites' })).toBeInTheDocument();
    expect(screen.getByText(/^Your invite links\./)).toBeInTheDocument();
    expect(screen.queryByRole('navigation')).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to isshoni' })).toBeInTheDocument();
    expect(await screen.findByRole('row', { name: /for Kim/ })).toBeInTheDocument();
  });

  it('hides every admin page from a member, and the Invites page from one who may not invite', async () => {
    renderAdmin({ path: '/admin/users', me: meFixture() });
    expect(await screen.findByRole('heading', { name: 'Nothing here' })).toBeInTheDocument();
  });

  it('shows "Not in this build yet" inside the layout for the pages a later slice adds', async () => {
    renderAdmin({ path: '/admin/doctor' });
    expect(await screen.findByRole('heading', { name: 'Not in this build yet' })).toBeInTheDocument();
    expect(screen.getByRole('navigation', { name: 'Admin' })).toBeInTheDocument();
    expect(screen.getAllByRole('main')).toHaveLength(1);
  });
});
