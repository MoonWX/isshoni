// The account menu in the desktop app (M2 docs/m2/03 §7.5, §8.3): Settings, Switch server with more than one server,
// Account ↗, My invites ↗ and Admin ↗ in the system browser, and "Sign out of <server>" through Auth.Unlink(), never
// POST /auth/logout and no Notifications. The browser's menu is RoomHeader.test.tsx's, unchanged.
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import { createDesktopTestPlatform, TEST_SERVER_ID, type DesktopTestPlatform } from '../app/testing/desktop';
import type { ServerInfo } from '../platform/desktop/bridge';
import { BridgeError } from '../platform/desktop/errors';
import { queryKeys } from '../protocol/queryKeys';
import { apiPath, meFixture, server } from '../test/msw';
import { testOrigin } from '../test/platform';
import { createTestServices, renderRoute } from '../test/render';
import { AccountMenu } from './AccountMenu';

function serverInfo(over: Partial<ServerInfo> = {}): ServerInfo {
  return {
    origin: testOrigin(),
    name: 'Test server',
    addedAt: '2026-10-01T12:00:00Z',
    user: { id: 'u1', username: 'alex', role: 'user' },
    device: { id: 'd1', name: 'Gaming PC' },
    id: TEST_SERVER_ID,
    active: true,
    linked: true,
    ...over,
  };
}

const OTHER: ServerInfo = serverInfo({
  origin: 'https://other.example.com',
  name: 'Other',
  id: 'x7w4k2m9',
  active: false,
});

/** Renders the menu in the desktop app; servers is what Auth.Servers() answers. */
function renderDesktopMenu({
  admin = false,
  createInvites = admin,
  servers = [serverInfo()],
  d = createDesktopTestPlatform(),
}: { admin?: boolean; createInvites?: boolean; servers?: ServerInfo[]; d?: DesktopTestPlatform } = {}) {
  d.fake.answers['auth.servers'] = servers;
  const me = meFixture({ admin, createInvites });
  const services = createTestServices({ platform: d.platform });
  services.queryClient.setQueryData(queryKeys.me, me);
  // The desktop never POSTs /auth/logout: any such request is recorded and fails the test below.
  const logouts: string[] = [];
  server.use(
    http.post(apiPath('/api/v1/auth/logout'), () => {
      logouts.push('logout');
      return new HttpResponse(null, { status: 204 });
    }),
  );
  const result = renderRoute({
    path: '/r/lounge',
    services,
    routes: [
      { path: '/r/:roomId', element: <AccountMenu me={me} /> },
      { path: '/desktop/settings', element: <h1>settings</h1> },
      { path: '*', element: <h1>another page</h1> },
    ],
  });
  return { ...result, d, services, logouts };
}

const openMenu = () => userEvent.click(screen.getByRole('button', { name: /^Account menu/ }));
const menu = () => within(screen.getByRole('dialog', { name: 'Account menu' }));

/** The menu's entries by their accessible names, links with their href. */
function entries(): Record<string, string | null> {
  const m = menu();
  const pairs: [string, string | null][] = [
    ...m.getAllByRole('link').map((a): [string, string | null] => [a.textContent, a.getAttribute('href')]),
    ...m.getAllByRole('button').map((b): [string, string | null] => [b.textContent, 'button']),
  ];
  return Object.fromEntries(pairs);
}

describe('the account menu in the desktop app (docs/m2/03 §7.5)', () => {
  it('offers Settings, Account in the browser, About and Sign out of the server: no Notifications', async () => {
    renderDesktopMenu();
    await openMenu();
    await waitFor(() => {
      expect(menu().getByRole('button', { name: 'Sign out of Test server' })).toBeInTheDocument();
    });
    expect(entries()).toEqual({
      Settings: '/desktop/settings',
      'Account (opens in your browser)': 'button',
      'About isshoni': '/about',
      'Sign out of Test server': 'button',
    });
    expect(menu().queryByRole('link', { name: 'Notifications' })).not.toBeInTheDocument();
    // One server: nothing to switch to.
    expect(menu().queryByRole('link', { name: 'Switch server' })).not.toBeInTheDocument();
  });

  it('offers "Switch server" (Settings → Servers) with more than one server', async () => {
    renderDesktopMenu({ servers: [serverInfo(), OTHER] });
    await openMenu();
    expect(await menu().findByRole('link', { name: 'Switch server' })).toHaveAttribute(
      'href',
      '/desktop/settings#servers',
    );
  });

  it('names the server as Auth.Servers() does', async () => {
    renderDesktopMenu({ servers: [serverInfo({ name: 'Friends' }), OTHER] });
    await openMenu();
    expect(await menu().findByRole('button', { name: 'Sign out of Friends' })).toBeInTheDocument();
  });

  it('Settings goes to /desktop/settings', async () => {
    renderDesktopMenu();
    await openMenu();
    await userEvent.click(menu().getByRole('link', { name: 'Settings' }));
    expect(await screen.findByRole('heading', { name: 'settings' })).toBeInTheDocument();
  });

  it('opens Account, My invites and Admin in the system browser', async () => {
    const member = renderDesktopMenu({ createInvites: true });
    await openMenu();
    expect(menu().queryByRole('button', { name: /^Admin/ })).not.toBeInTheDocument();
    await userEvent.click(menu().getByRole('button', { name: 'My invites (opens in your browser)' }));
    expect(screen.queryByRole('dialog', { name: 'Account menu' })).not.toBeInTheDocument();
    await openMenu();
    await userEvent.click(menu().getByRole('button', { name: 'Account (opens in your browser)' }));
    expect(member.d.opened).toEqual([`${testOrigin()}/admin/invites`, `${testOrigin()}/account`]);
    member.unmount();

    const admin = renderDesktopMenu({ admin: true });
    await openMenu();
    // An admin's invites are under Admin.
    expect(menu().queryByRole('button', { name: /^My invites/ })).not.toBeInTheDocument();
    expect(menu().getByRole('button', { name: 'Invite friends' })).toHaveAttribute('aria-haspopup', 'dialog');
    await userEvent.click(menu().getByRole('button', { name: 'Admin (opens in your browser)' }));
    expect(admin.d.opened).toEqual([`${testOrigin()}/admin`]);
  });

  it('Sign out asks first; Cancel unlinks nothing', async () => {
    const { d } = renderDesktopMenu();
    await openMenu();
    await userEvent.click(await menu().findByRole('button', { name: 'Sign out of Test server' }));
    const confirm = screen.getByRole('dialog', { name: 'Sign out isshoni on this PC from Test server?' });
    expect(
      within(confirm).getByText('The server stays in your list as Not linked. A share on it stops.'),
    ).toBeInTheDocument();
    await userEvent.click(within(confirm).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog', { name: /^Sign out isshoni/ })).not.toBeInTheDocument();
    expect(d.fake.callsOf('auth.unlink')).toEqual([]);
  });

  it('Sign out calls Auth.Unlink(), never POST /auth/logout, and forgets the user here', async () => {
    const { d, services, logouts } = renderDesktopMenu();
    d.fake.answers['auth.unlink'] = { confirmed: true };
    await openMenu();
    await userEvent.click(await menu().findByRole('button', { name: 'Sign out of Test server' }));
    await userEvent.click(
      within(screen.getByRole('dialog', { name: /^Sign out isshoni/ })).getByRole('button', { name: 'Sign out' }),
    );
    await waitFor(() => {
      expect(services.queryClient.getQueryData(queryKeys.me)).toBeNull();
    });
    expect(d.fake.callsOf('auth.unlink')).toEqual([[]]);
    expect(logouts).toEqual([]);
    expect(services.ui.getState().toasts).toEqual([]);
  });

  it('a sign-out the server didn’t confirm says to remove the device on the Devices page, in a toast that stays', async () => {
    const { d, services, logouts } = renderDesktopMenu();
    d.fake.answers['auth.unlink'] = { confirmed: false };
    await openMenu();
    await userEvent.click(await menu().findByRole('button', { name: 'Sign out of Test server' }));
    await userEvent.click(
      within(screen.getByRole('dialog', { name: /^Sign out isshoni/ })).getByRole('button', { name: 'Sign out' }),
    );
    await waitFor(() => {
      expect(services.queryClient.getQueryData(queryKeys.me)).toBeNull();
    });
    expect(services.ui.getState().toasts).toMatchObject([
      { kind: 'info', message: 'Remove Gaming PC on your Devices page to finish.', durationMs: null },
    ]);
    expect(logouts).toEqual([]);
  });

  it('a refused Unlink keeps the user signed in, with an error toast', async () => {
    const { d, services } = renderDesktopMenu();
    d.fake.answers['auth.unlink'] = () => Promise.reject(new BridgeError('internal'));
    await openMenu();
    await userEvent.click(await menu().findByRole('button', { name: 'Sign out of Test server' }));
    await userEvent.click(
      within(screen.getByRole('dialog', { name: /^Sign out isshoni/ })).getByRole('button', { name: 'Sign out' }),
    );
    await waitFor(() => {
      expect(services.ui.getState().toasts).toMatchObject([{ kind: 'error' }]);
    });
    expect(services.queryClient.getQueryData(queryKeys.me)).not.toBeNull();
  });
});
