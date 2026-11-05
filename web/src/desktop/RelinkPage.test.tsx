// The relink page and the server switcher (docs/m2/03 §8.1, §8.3; README WA71) over the fake NativeBridge.
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';

import { createDesktopTestPlatform } from '../app/testing/desktop';
import type { AuthNotice, ServerInfo } from '../platform/desktop/bridge';
import { BridgeError } from '../platform/desktop/errors';
import { authState } from '../platform/desktop/testing/fakeBridge';
import { testOrigin } from '../test/platform';
import { renderDesktopPage } from './testing/render';

const B = 'https://b.example.com';

function servers(): ServerInfo[] {
  return [
    {
      origin: testOrigin(),
      name: 'Test server',
      addedAt: '2026-11-01T10:00:00Z',
      user: { id: 'u1', username: 'alex', role: 'user' },
      device: { id: 'd1', name: 'Test PC' },
      id: 'k3m9p2qx',
      active: true,
      linked: true,
    },
    {
      origin: B,
      name: '',
      addedAt: '2026-11-02T10:00:00Z',
      user: null,
      device: null,
      id: 'b0b0b0b0',
      active: false,
      linked: false,
    },
  ];
}

/** The relink page for a notice. */
function relink(notice: AuthNotice) {
  const d = createDesktopTestPlatform();
  d.fake.answers['auth.state'] = { ...authState(testOrigin(), 'unlinked'), notice };
  d.fake.answers['auth.servers'] = servers();
  return renderDesktopPage('/desktop/relink', d);
}

afterEach(() => {
  globalThis.history.replaceState(null, '', '/');
});

describe('RelinkPage', () => {
  it.each<[AuthNotice, string]>([
    ['signed_out', 'This PC was signed out of Test server (removed on the Devices page, or not used for 90 days)'],
    [
      'refresh_reused',
      "Someone else used this PC's sign-in, so Test server signed it out. Link again and check your Devices page",
    ],
    ['account_disabled', 'Your account on Test server is disabled'],
  ])('%s: says why, and offers Link again, Switch server and Remove server', async (notice, text) => {
    relink(notice);
    expect(await screen.findByText(text)).toHaveAttribute('role', 'status');
    expect(screen.getByRole('heading', { name: 'Link this PC to Test server again' })).toBeInTheDocument();
    for (const name of ['Link again', 'Switch server', 'Remove server']) {
      expect(screen.getByRole('button', { name })).toBeInTheDocument();
    }
  });

  it('Link again calls Auth.Link() and goes to the link step', async () => {
    const user = userEvent.setup();
    const { d, router } = relink('signed_out');
    await user.click(await screen.findByRole('button', { name: 'Link again' }));
    expect(d.fake.callsOf('auth.link')).toHaveLength(1);
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/desktop/setup');
    });
  });

  it('a Link again error stays with its text', async () => {
    const user = userEvent.setup();
    const { d } = relink('signed_out');
    d.fake.answers['auth.link'] = () => Promise.reject(new BridgeError('server_unreachable'));
    await user.click(await screen.findByRole('button', { name: 'Link again' }));
    expect(await screen.findByRole('alert')).toHaveTextContent("isshoni can't reach this server.");
  });

  it('Remove server removes this server; the window then reloads at /', async () => {
    const user = userEvent.setup();
    const { d } = relink('account_disabled');
    globalThis.history.replaceState(null, '', '/desktop/relink');
    await user.click(await screen.findByRole('button', { name: 'Remove server' }));
    expect(d.fake.callsOf('auth.removeServer')).toEqual([[testOrigin()]]);
    expect(globalThis.location.pathname).toBe('/');
  });

  it('follows auth.state: a link that starts goes to the link step', async () => {
    const { d, router } = relink('signed_out');
    await screen.findByRole('button', { name: 'Link again' });
    act(() => {
      d.fake.emit('auth.state', authState(testOrigin(), 'linking'));
    });
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/desktop/setup');
    });
  });
});

describe('ServerSwitcher', () => {
  async function openSwitcher() {
    const user = userEvent.setup();
    const r = relink('signed_out');
    await user.click(await screen.findByRole('button', { name: 'Switch server' }));
    const list = await screen.findByRole('region', { name: 'Your servers' });
    return { user, list, ...r };
  }

  it('lists the servers: name or host, linked as or Not linked, the active one', async () => {
    const { list } = await openSwitcher();
    const items = within(list).getAllByRole('listitem');
    expect(items).toHaveLength(2);
    expect(items[0]).toHaveTextContent(`Test server${new URL(testOrigin()).host} · Linked as alex · Active`);
    expect(items[1]).toHaveTextContent('b.example.comb.example.com · Not linked');
    expect(within(items[0] as HTMLElement).queryByRole('button', { name: 'Switch' })).not.toBeInTheDocument();
  });

  it('Switch calls Auth.SwitchServer, with / as the address the window reloads at', async () => {
    const { user, list, d } = await openSwitcher();
    globalThis.history.replaceState(null, '', '/desktop/relink');
    await user.click(within(list).getByRole('button', { name: 'Switch' }));
    expect(d.fake.callsOf('auth.switchServer')).toEqual([[B]]);
    expect(globalThis.location.pathname).toBe('/');
  });

  it('a refused switch (busy) says so, and the address stays', async () => {
    const { user, list, d } = await openSwitcher();
    d.fake.answers['auth.switchServer'] = () => Promise.reject(new BridgeError('busy'));
    globalThis.history.replaceState(null, '', '/desktop/relink');
    await user.click(within(list).getByRole('button', { name: 'Switch' }));
    expect(await within(list).findByRole('alert')).toHaveTextContent('Stop sharing first.');
    expect(globalThis.location.pathname).toBe('/desktop/relink');
  });

  it('Sign out asks first, then calls Auth.Unlink(); an unconfirmed revoke names the device', async () => {
    const { user, list, d } = await openSwitcher();
    d.fake.answers['auth.unlink'] = { confirmed: false };
    await user.click(within(list).getByRole('button', { name: 'Sign out of Test server' }));
    const dialog = screen.getByRole('dialog', { name: 'Sign out isshoni on this PC from Test server?' });
    expect(d.fake.callsOf('auth.unlink')).toEqual([]);
    await user.click(within(dialog).getByRole('button', { name: 'Sign out' }));
    expect(d.fake.callsOf('auth.unlink')).toHaveLength(1);
    expect(await within(list).findByText('Remove Test PC on your Devices page to finish.')).toBeInTheDocument();
  });

  it('Remove asks first, then removes another server without a reload address', async () => {
    const { user, list, d } = await openSwitcher();
    globalThis.history.replaceState(null, '', '/desktop/relink');
    const items = within(list).getAllByRole('listitem');
    await user.click(within(items[1] as HTMLElement).getByRole('button', { name: 'Remove' }));
    const dialog = screen.getByRole('dialog', { name: 'Remove b.example.com?' });
    await user.click(within(dialog).getByRole('button', { name: 'Remove' }));
    expect(d.fake.callsOf('auth.removeServer')).toEqual([[B]]);
    expect(globalThis.location.pathname).toBe('/desktop/relink');
  });

  it('Add a server opens the wizard’s server step', async () => {
    const { user, list, router } = await openSwitcher();
    await user.click(within(list).getByRole('button', { name: 'Add a server' }));
    expect(router.state.location.pathname + router.state.location.search).toBe('/desktop/setup?new=1');
  });
});
