// The wizard's server, add, confirm and link steps (docs/m2/03 §8.1, §8.2; README WA71) over the fake NativeBridge.
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createDesktopTestPlatform } from '../app/testing/desktop';
import type { AuthState } from '../platform/desktop/bridge';
import { BridgeError } from '../platform/desktop/errors';
import { authState } from '../platform/desktop/testing/fakeBridge';
import { testOrigin } from '../test/platform';
import { DESKTOP_LINKING_HELP } from './SetupPage';
import { renderDesktopPage } from './testing/render';

const NEW = 'https://watch.example.com';

/** The link of a code being shown, as Go builds it (02 §11.2). */
function linking(origin: string): AuthState {
  return {
    ...authState(origin, 'linking'),
    link: {
      userCode: 'WDJB-MJHT',
      verificationUri: `${origin}/link`,
      verificationUriComplete: `${origin}/link?code=WDJB-MJHT`,
      expiresAt: Date.now() + 600_000,
    },
  };
}

afterEach(() => {
  globalThis.history.replaceState(null, '', '/');
  vi.useRealTimers();
});

describe('server step', () => {
  it('leads with the isshoni page, takes an address, and shows the probe’s name and host on the confirm step', async () => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform({ server: null });
    d.fake.answers['auth.probe'] = { origin: NEW, name: 'Friends', version: '0.2.0' };
    renderDesktopPage('/desktop/setup', d);

    expect(await screen.findByText(/Installed from your isshoni page\? Go back to it and click/)).toBeInTheDocument();
    const field = screen.getByLabelText('Your isshoni server');
    expect(field).toHaveFocus();
    expect(field).toHaveAttribute('autocomplete', 'off');
    await user.type(field, 'watch.example.com/r/lounge{Enter}');

    expect(
      await screen.findByRole('heading', { name: 'Link isshoni to Friends (watch.example.com)?' }),
    ).toBeInTheDocument();
    expect(d.fake.callsOf('auth.probe')).toEqual([['watch.example.com/r/lounge']]);
    // The same host as typed: no "calls itself" line.
    expect(screen.queryByText(/This server calls itself/)).not.toBeInTheDocument();
    expect(screen.getByText(/there's no recording and no telemetry/)).toBeInTheDocument();
  });

  it('says so when the server calls itself another host', async () => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform({ server: null });
    d.fake.answers['auth.probe'] = { origin: NEW, name: 'Friends', version: '0.2.0' };
    renderDesktopPage('/desktop/setup', d);
    await user.type(await screen.findByLabelText('Your isshoni server'), 'friends.example.org{Enter}');
    expect(await screen.findByText('This server calls itself watch.example.com.')).toBeInTheDocument();
  });

  it.each([
    ['invalid_address', {}, "That doesn't look like a server address. Check it and try again."],
    ['server_unreachable', {}, "isshoni can't reach this server. Check the address and your connection."],
    ['tls_untrusted', {}, "This server's certificate isn't trusted, so isshoni won't connect to it."],
    ['not_isshoni', {}, "That address isn't an isshoni server."],
    ['server_not_set_up', {}, "This server isn't set up yet. Its admin has to finish the setup in a browser first."],
    [
      'incompatible_server',
      { verdict: 'update_app', reason: 'client_too_old', serverVersion: '0.3.0', minClientVersion: '0.3.0' },
      'This server needs a newer isshoni app. You have 0.2.0; the server runs 0.3.0.',
    ],
    [
      'incompatible_server',
      { verdict: 'update_server', reason: 'no_device_flow', serverVersion: '0.1.4', minClientVersion: '' },
      'This server runs isshoni 0.1.4, which is too old for this app. Ask your admin to update it.',
    ],
    ['internal', {}, 'Something went wrong in isshoni. Try again.'],
  ])('shows the text of the probe error %s under the field', async (code, params, text) => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform({ server: null });
    d.fake.answers['auth.probe'] = () => Promise.reject(new BridgeError(code, params));
    renderDesktopPage('/desktop/setup', d);
    const field = await screen.findByLabelText('Your isshoni server');
    await user.type(field, 'watch.example.com{Enter}');
    await waitFor(() => {
      expect(field).toHaveAccessibleDescription(expect.stringContaining(text));
    });
    expect(field).toHaveAttribute('aria-invalid', 'true');
  });

  it('server_unreachable offers "See why", which opens the project site’s section in the browser', async () => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform({ server: null });
    d.fake.answers['auth.probe'] = () => Promise.reject(new BridgeError('server_unreachable'));
    renderDesktopPage('/desktop/setup', d);
    await user.type(await screen.findByLabelText('Your isshoni server'), 'watch.example.com{Enter}');
    await user.click(await screen.findByRole('button', { name: 'Opens in your browser but not here? See why' }));
    expect(d.opened).toEqual([DESKTOP_LINKING_HELP]);
    expect(DESKTOP_LINKING_HELP).toMatch(/^https:\/\/.*troubleshooting#desktop-linking$/);
  });

  it('an invite or setup link gets the "open it in your browser first" hint, and no probe', async () => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform({ server: null });
    renderDesktopPage('/desktop/setup', d);
    await user.type(await screen.findByLabelText('Your isshoni server'), 'https://watch.example.com/invite#tok{Enter}');
    expect(
      await screen.findByText('This is an invite link. Open it in your browser first to create your account.'),
    ).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Open in browser' }));
    expect(d.opened).toEqual(['https://watch.example.com/invite#tok']);
    expect(d.fake.callsOf('auth.probe')).toEqual([]);
  });

  it('Cancel on the first server leaves "Link isshoni to your server" with Start', async () => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform({ server: null });
    renderDesktopPage('/desktop/setup', d);
    await user.click(await screen.findByRole('button', { name: 'Cancel' }));
    expect(screen.getByRole('heading', { name: 'Link isshoni to your server' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Start' }));
    expect(screen.getByLabelText('Your isshoni server')).toBeInTheDocument();
  });

  it('a link that arrives while it is open opens the add step', async () => {
    const d = createDesktopTestPlatform({ server: null });
    renderDesktopPage('/desktop/setup', d);
    await screen.findByLabelText('Your isshoni server');
    act(() => {
      d.fake.emit('deeplink.open', {
        add: { origin: NEW, host: 'watch.example.com', hostUnicode: 'watch.example.com', roomId: '' },
      });
    });
    expect(await screen.findByRole('heading', { name: 'Add server watch.example.com?' })).toBeInTheDocument();
  });

  it('too_many_servers from a link shows its text', async () => {
    const d = createDesktopTestPlatform({ server: null });
    d.fake.answers['app.pendingLink'] = { error: 'too_many_servers' };
    renderDesktopPage('/desktop/setup', d);
    expect(await screen.findByLabelText('Your isshoni server')).toHaveAccessibleDescription(
      "You've added 20 servers. Remove one first.",
    );
  });
});

describe('add step', () => {
  const add = {
    add: { origin: NEW, host: 'watch.example.com', hostUnicode: 'watch.example.com', roomId: 'lounge' },
  };

  it('asks "Add server {host}?" before any network request; Add probes and, for the same host, adds it', async () => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform({ server: null });
    d.fake.answers['app.pendingLink'] = add;
    d.fake.answers['auth.probe'] = { origin: NEW, name: 'Friends', version: '0.2.0' };
    renderDesktopPage('/desktop/setup', d);

    expect(await screen.findByRole('heading', { name: 'Add server watch.example.com?' })).toBeInTheDocument();
    expect(
      screen.getByText("Only add servers you trust. The server's admin can see what you share."),
    ).toBeInTheDocument();
    // Nothing but the pending link was asked: no probe, no request to the server.
    expect(d.fake.calls.map((c) => c.method)).toEqual(['app.pendingLink']);

    await user.click(screen.getByRole('button', { name: 'Add' }));
    await waitFor(() => {
      expect(d.fake.callsOf('auth.addServer')).toEqual([[NEW]]);
    });
    expect(d.fake.callsOf('auth.probe')).toEqual([[NEW]]);
    // The window reloads at /desktop/setup?link=1 (Go reloads the window's current address).
    expect(globalThis.location.pathname + globalThis.location.search).toBe('/desktop/setup?link=1');
  });

  it('shows hostUnicode (host) against look-alike names', async () => {
    const d = createDesktopTestPlatform({ server: null });
    d.fake.answers['app.pendingLink'] = {
      add: {
        origin: 'https://xn--bcher-kva.example',
        host: 'xn--bcher-kva.example',
        hostUnicode: 'bücher.example',
        roomId: '',
      },
    };
    renderDesktopPage('/desktop/setup', d);
    expect(
      await screen.findByRole('heading', { name: 'Add server bücher.example (xn--bcher-kva.example)?' }),
    ).toBeInTheDocument();
  });

  it('a server that calls itself another host gets the confirm step first', async () => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform({ server: null });
    d.fake.answers['app.pendingLink'] = add;
    d.fake.answers['auth.probe'] = { origin: 'https://other.example.net', name: 'Friends', version: '0.2.0' };
    renderDesktopPage('/desktop/setup', d);
    await user.click(await screen.findByRole('button', { name: 'Add' }));
    expect(await screen.findByText('This server calls itself other.example.net.')).toBeInTheDocument();
    expect(d.fake.callsOf('auth.addServer')).toEqual([]);
    await user.click(screen.getByRole('button', { name: 'Link' }));
    // Confirmed under the add question's origin, so Go answers the question (Servers.Add adopts the other host).
    await waitFor(() => {
      expect(d.fake.callsOf('auth.addServer')).toEqual([[NEW]]);
    });
  });

  it('a probe error stays on the add step with Try again; Cancel answers the link', async () => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform({ server: null });
    d.fake.answers['app.pendingLink'] = add;
    d.fake.answers['auth.probe'] = () => Promise.reject(new BridgeError('not_isshoni'));
    renderDesktopPage('/desktop/setup', d);
    await user.click(await screen.findByRole('button', { name: 'Add' }));
    expect(await screen.findByRole('alert')).toHaveTextContent("That address isn't an isshoni server.");
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(d.fake.callsOf('app.linkHandled')).toHaveLength(1);
    expect(screen.getByRole('button', { name: 'Start' })).toBeInTheDocument();
  });

  it.each([
    ['too_many_servers', "You've added 20 servers. Remove one first."],
    ['busy', 'Stop sharing first.'],
    ['server_unreachable', "isshoni can't reach this server. Check the address and your connection."],
  ])('an AddServer refusal (%s) comes back to the step with its text', async (code, text) => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform({ server: null });
    d.fake.answers['app.pendingLink'] = add;
    d.fake.answers['auth.probe'] = { origin: NEW, name: 'Friends', version: '0.2.0' };
    d.fake.answers['auth.addServer'] = () => Promise.reject(new BridgeError(code));
    globalThis.history.replaceState(null, '', '/desktop/setup');
    renderDesktopPage('/desktop/setup', d);
    await user.click(await screen.findByRole('button', { name: 'Add' }));
    await waitFor(() => {
      expect(d.fake.callsOf('auth.addServer')).toEqual([[NEW]]);
    });
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    // The address the reload would have used is put back.
    expect(globalThis.location.pathname + globalThis.location.search).toBe('/desktop/setup');
  });
});

describe('link step', () => {
  it('?link=1: drops the parameter and calls Auth.Link() once, when unlinked without a notice', async () => {
    const d = createDesktopTestPlatform();
    d.setAuth('unlinked');
    const { router } = renderDesktopPage('/desktop/setup?link=1', d);
    await waitFor(() => {
      expect(d.fake.callsOf('auth.link')).toHaveLength(1);
    });
    expect(router.state.location.search).toBe('');
    expect(d.fake.callsOf('app.pendingLink')).toEqual([]);
  });

  it('?link=1 starts nothing when the state has a notice', async () => {
    const d = createDesktopTestPlatform();
    d.fake.answers['auth.state'] = { ...authState(testOrigin(), 'unlinked'), notice: 'denied' };
    renderDesktopPage('/desktop/setup?link=1', d);
    expect(await screen.findByText('Linking was declined in the browser.')).toBeInTheDocument();
    expect(d.fake.callsOf('auth.link')).toEqual([]);
  });

  it('shows the code, the QR code of verificationUriComplete, the short URL, the countdown and the rename note', async () => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform();
    const st = linking(testOrigin());
    d.fake.answers['auth.state'] = st;
    renderDesktopPage('/desktop/setup', d);

    expect(await screen.findByTestId('link-code')).toHaveTextContent('WDJB-MJHT');
    const host = new URL(testOrigin()).host;
    expect(await screen.findByRole('heading', { name: `Test server (${host})` })).toBeInTheDocument();
    expect(screen.getByText(`or open ${host}/link and enter WDJB-MJHT`)).toBeInTheDocument();
    expect(screen.getByText(/^Expires in (10:00|9:\d\d)$/)).toBeInTheDocument();
    expect(
      screen.getByText('You can rename this PC on the approval page or later on your Devices page.'),
    ).toBeInTheDocument();
    const qr = screen.getByRole('img', { name: 'QR code of the link page' });
    expect(qr.querySelector('path')?.getAttribute('d')).toMatch(/^M\d/);
    // No name field: the app sends 02's default name.
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Open browser again' }));
    expect(d.opened).toEqual([st.link?.verificationUriComplete]);

    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(globalThis.navigator, 'clipboard', { configurable: true, value: { writeText } });
    await user.click(screen.getByRole('button', { name: 'Copy link' }));
    expect(writeText).toHaveBeenCalledWith(st.link?.verificationUriComplete);
    expect(await screen.findByText('Link copied')).toBeInTheDocument();
  });

  it('linked: "Linked as …", then the pending room', async () => {
    const d = createDesktopTestPlatform();
    d.fake.answers['auth.state'] = linking(testOrigin());
    d.fake.answers['app.pendingLink'] = { roomId: 'lounge' };
    const { router } = renderDesktopPage('/desktop/setup?link=1', d);
    await screen.findByTestId('link-code');
    act(() => {
      d.fake.emit('auth.state', authState(testOrigin(), 'linked'));
    });
    expect(await screen.findByRole('heading', { name: 'room lounge' })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/r/lounge');
  });

  it('save_failed: the banner, and Continue', async () => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform();
    d.fake.answers['auth.state'] = { ...authState(testOrigin(), 'linked'), notice: 'save_failed' };
    d.fake.answers['app.pendingLink'] = null;
    const { router } = renderDesktopPage('/desktop/setup?link=1', d);
    expect(await screen.findByText('Linked as alex')).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent(
      "Couldn't save the sign-in to Windows Credential Manager. You'll have to link again after restarting the app.",
    );
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/');
    });
  });

  it.each<[AuthState['notice'], string, string]>([
    ['denied', 'Linking was declined in the browser.', 'Try again'],
    ['code_expired', 'The code expired.', 'Get a new code'],
  ])('the notice %s: its text, and its button calls Auth.Link()', async (notice, text, button) => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform();
    d.fake.answers['auth.state'] = { ...authState(testOrigin(), 'unlinked'), notice };
    renderDesktopPage('/desktop/setup', d);
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    await user.click(screen.getByRole('button', { name: button }));
    expect(d.fake.callsOf('auth.link')).toHaveLength(1);
  });

  it.each([
    ['server_unreachable', {}, "isshoni can't reach this server. Check the address and your connection."],
    ['rate_limited', {}, 'Too many tries. Wait a moment and try again.'],
    // 02's ErrBadResponse (a verification URI that failed the check) is internal {reason: bad_response}: the
    // generic link error, as is any other code the step has no text for.
    ['internal', { reason: 'bad_response' }, "Couldn't start linking with this server"],
    ['internal', {}, "Couldn't start linking with this server"],
    ['something_new', {}, "Couldn't start linking with this server"],
  ])('a Link() error (%s): its text with Try again', async (code, params, text) => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform();
    d.setAuth('unlinked');
    d.fake.answers['auth.link'] = () => Promise.reject(new BridgeError(code, params));
    renderDesktopPage('/desktop/setup', d);
    await user.click(await screen.findByRole('button', { name: 'Link' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
  });

  it('incompatible: the version text and Try again (Auth.Recheck)', async () => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform();
    const st: AuthState = {
      ...authState(testOrigin(), 'incompatible'),
      compat: { verdict: 'update_server', reason: 'server_too_old', serverVersion: '0.1.4', minClientVersion: '' },
    };
    d.fake.answers['auth.state'] = st;
    d.fake.answers['auth.recheck'] = st;
    renderDesktopPage('/desktop/setup', d);
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'This server runs isshoni 0.1.4, which is too old for this app. Ask your admin to update it.',
    );
    await user.click(screen.getByRole('button', { name: 'Try again' }));
    expect(d.fake.callsOf('auth.recheck')).toHaveLength(1);
  });

  it('leaving the step while a code shows calls Auth.CancelLink()', async () => {
    const d = createDesktopTestPlatform();
    d.fake.answers['auth.state'] = linking(testOrigin());
    const { unmount } = renderDesktopPage('/desktop/setup', d);
    await screen.findByTestId('link-code');
    unmount();
    expect(d.fake.callsOf('auth.cancelLink')).toHaveLength(1);
  });

  it('ignores a state of another server: the window is about to reload for it', async () => {
    const d = createDesktopTestPlatform();
    d.fake.answers['auth.state'] = linking(testOrigin());
    renderDesktopPage('/desktop/setup', d);
    await screen.findByTestId('link-code');
    act(() => {
      d.fake.emit('auth.state', authState('https://b.example.com', 'linked'));
    });
    expect(screen.getByTestId('link-code')).toBeInTheDocument();
  });
});

describe('Sign in with username and password instead', () => {
  async function openForm() {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform();
    d.fake.answers['auth.state'] = linking(testOrigin());
    const r = renderDesktopPage('/desktop/setup', d);
    await screen.findByTestId('link-code');
    await user.click(screen.getByRole('button', { name: 'Sign in with username and password instead' }));
    return { user, ...r };
  }

  it('has username and password with autocomplete off, and no name field', async () => {
    await openForm();
    const form = screen.getByRole('heading', { name: 'Sign in with your username and password' }).closest('form');
    expect(form).not.toBeNull();
    const username = screen.getByLabelText('Username');
    const password = screen.getByLabelText('Password');
    expect(username).toHaveAttribute('autocomplete', 'off');
    expect(password).toHaveAttribute('autocomplete', 'off');
    expect(password).toHaveAttribute('type', 'password');
    expect(within(form as HTMLElement).getAllByRole('textbox')).toHaveLength(1);
  });

  it('a wrong password: its text on the form, the password field cleared; the right one links', async () => {
    const { user, d, router } = await openForm();
    let attempt = 0;
    d.fake.answers['auth.linkWithPassword'] = () =>
      ++attempt === 1
        ? Promise.reject(new BridgeError('invalid_credentials'))
        : Promise.resolve(authState(testOrigin(), 'linked'));
    d.fake.answers['app.pendingLink'] = null;

    await user.type(screen.getByLabelText('Username'), 'alex');
    await user.type(screen.getByLabelText('Password'), 'wrong-password');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Wrong username or password.');
    expect(screen.getByLabelText('Password')).toHaveValue('');
    expect(screen.getByLabelText('Username')).toHaveValue('alex');

    await user.type(screen.getByLabelText('Password'), 'right-password');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/');
    });
    expect(d.fake.callsOf('auth.linkWithPassword')).toEqual([
      ['alex', 'wrong-password'],
      ['alex', 'right-password'],
    ]);
  });

  it.each([
    ['account_pending', {}, "An admin hasn't approved your account yet. Try logging in later."],
    ['account_disabled', {}, 'Your account was disabled. Ask an admin.'],
    ['rate_limited', { retryAfter: 12 }, 'Too many attempts. Try again in 12 seconds.'],
    ['server_busy', {}, 'The server is busy. Try again in a moment.'],
    ['server_unreachable', {}, "isshoni can't reach this server. Check the address and your connection."],
  ])('%s: its text stays on the form', async (code, params, text) => {
    const { user, d } = await openForm();
    d.fake.answers['auth.linkWithPassword'] = () => Promise.reject(new BridgeError(code, params));
    await user.type(screen.getByLabelText('Username'), 'alex');
    await user.type(screen.getByLabelText('Password'), 'pw');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(screen.getByLabelText('Password')).toHaveValue('');
  });

  it('validation_failed: M1’s field text under the field', async () => {
    const { user, d } = await openForm();
    d.fake.answers['auth.linkWithPassword'] = () =>
      Promise.reject(new BridgeError('validation_failed', { fields: { username: 'too_long' } }));
    await user.type(screen.getByLabelText('Username'), 'a'.repeat(40));
    await user.type(screen.getByLabelText('Password'), 'pw');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    const username = screen.getByLabelText('Username');
    await waitFor(() => {
      expect(username).toHaveAttribute('aria-invalid', 'true');
    });
    expect(username).not.toHaveAccessibleDescription('');
    expect(screen.getByRole('alert')).toHaveTextContent('Check the highlighted fields.');
  });

  it('Back returns to the code (Auth.Link())', async () => {
    const { user, d } = await openForm();
    await user.click(screen.getByRole('button', { name: 'Back' }));
    expect(d.fake.callsOf('auth.link')).toHaveLength(1);
    expect(await screen.findByTestId('link-code')).toBeInTheDocument();
  });
});
