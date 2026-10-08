// /account/devices (05 §15.2) against MSW: the list of browser sessions (03 §12.3 #14), revoking one (#15), "Sign
// out other browsers" (#16), "Log out everywhere" (#4), this browser's Sign out, and the linked devices (#17, #18),
// which M1's server always answers with an empty list.
import { screen, waitFor, within } from '@testing-library/react';
import userEvent, { type UserEvent } from '@testing-library/user-event';
import { HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { CHANNEL_NAME } from '../app/session';
import { networkError, noContent, onPost } from '../auth/testing/harness';
import { formatDateTime } from '../lib/time';
import type { SessionInfo } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { apiError } from '../test/msw';
import { lastSeenText, sortSessions } from './DevicesPage';
import {
  deviceFixture,
  LOGIN_HEADING,
  mockDevices,
  mockSessions,
  onDelete,
  onGet,
  renderAccount,
  sessionFixture,
} from './testing/harness';

const HEADING = 'Devices';
const MINUTE = 60_000;
const DAY = 24 * 60 * MINUTE;

// Three sessions of one user. Their times are relative to now, so they are made anew for every test: a slow run
// must not turn "3 minutes ago" into 4.
let THIS: SessionInfo;
let PHONE: SessionInfo;
let LAPTOP: SessionInfo;
/** As the server lists them: by creation, not the order the page shows. */
let ALL: SessionInfo[];

beforeEach(() => {
  THIS = sessionFixture({ id: 's1s2s3s4s5s6', name: 'Chrome on macOS', current: true, seenAgoMs: 4 * MINUTE });
  PHONE = sessionFixture({
    id: 'z9x8c7v6b5n4',
    name: 'Safari on iPhone',
    lastIp: '198.51.100.23',
    seenAgoMs: 3 * MINUTE,
  });
  LAPTOP = sessionFixture({
    id: 'p0o9i8u7y6t5',
    name: 'Firefox on Linux',
    lastIp: '2001:db8::1',
    seenAgoMs: 2 * DAY,
  });
  ALL = [LAPTOP, PHONE, THIS];
});

async function ready(): Promise<UserEvent> {
  await screen.findByRole('heading', { level: 1, name: HEADING });
  return userEvent.setup();
}

function browsers(): HTMLElement {
  return screen.getByRole('region', { name: 'Browsers' });
}

/** The rows of the browsers list, once it has loaded. */
async function rows(): Promise<HTMLElement[]> {
  return within(browsers()).findAllByRole('listitem');
}

function names(items: HTMLElement[]): (string | null | undefined)[] {
  return items.map((li) => li.querySelector('p span')?.textContent);
}

describe('sortSessions', () => {
  it('puts this browser first, then the most recently seen', () => {
    expect(sortSessions(ALL).map((s) => s.name)).toEqual(['Chrome on macOS', 'Safari on iPhone', 'Firefox on Linux']);
    // A session without a time (the server sends none before the first touch) goes last, and nothing is mutated.
    const never = { ...PHONE, id: 'n0n0n0n0n0n0', name: 'Edge on Windows', lastSeenAt: undefined };
    expect(sortSessions([never, LAPTOP]).map((s) => s.name)).toEqual(['Firefox on Linux', 'Edge on Windows']);
    expect(ALL.map((s) => s.name)).toEqual(['Firefox on Linux', 'Safari on iPhone', 'Chrome on macOS']);
  });
});

describe('lastSeenText', () => {
  const now = new Date('2026-10-08T12:00:00.000Z');
  const ago = (ms: number): Date => new Date(now.getTime() - ms);

  it('says how long ago, in the largest whole unit', () => {
    expect(lastSeenText(ago(3 * MINUTE), 'en', now)).toBe('3 minutes ago');
    expect(lastSeenText(ago(5 * 60 * MINUTE), 'en', now)).toBe('5 hours ago');
    expect(lastSeenText(ago(DAY), 'en', now)).toBe('yesterday');
    expect(lastSeenText(ago(20 * DAY), 'en', now)).toBe('2 weeks ago');
    expect(lastSeenText(now, 'en', now)).toBe('now');
  });

  it('reads "now" for a time ahead of this device\'s clock, never "in 20 seconds"', () => {
    expect(lastSeenText(ago(-20_000), 'en', now)).toBe('now');
    expect(lastSeenText(ago(-2 * DAY), 'en', now)).toBe('now');
  });
});

describe('DevicesPage: browsers', () => {
  it('lists the sessions: name, last seen and address, with this browser first and marked', async () => {
    mockSessions(ALL);
    mockDevices();
    renderAccount('/account/devices');
    await ready();
    expect(within(browsers()).getByText("Every browser where you're signed in to Test server.")).toBeInTheDocument();

    const [mine, phone, laptop, ...rest] = await rows();
    expect(rest).toEqual([]);
    if (!mine || !phone || !laptop) throw new Error('expected three rows');

    // This browser: marked, with Sign out instead of Revoke.
    expect(within(mine).getByText('Chrome on macOS')).toBeInTheDocument();
    expect(within(mine).getByText('This browser')).toBeInTheDocument();
    expect(within(mine).getByText('Active now')).toBeInTheDocument();
    expect(within(mine).getByText('203.0.113.7')).toBeInTheDocument();
    expect(within(mine).getByRole('button', { name: 'Sign out' })).toHaveAccessibleDescription('Chrome on macOS');
    expect(within(mine).queryByRole('button', { name: 'Revoke' })).not.toBeInTheDocument();

    // Another browser: when it was last seen (the exact time as a tooltip), from where, and Revoke.
    expect(within(phone).getByText('Safari on iPhone')).toBeInTheDocument();
    expect(within(phone).queryByText('This browser')).not.toBeInTheDocument();
    const seen = within(phone).getByText('Last active 3 minutes ago');
    expect(seen.tagName).toBe('TIME');
    expect(seen).toHaveAttribute('datetime', PHONE.lastSeenAt);
    expect(seen).toHaveAttribute('title', formatDateTime(new Date(PHONE.lastSeenAt ?? ''), 'en'));
    expect(within(phone).getByText('198.51.100.23')).toBeInTheDocument();
    // Every row's button says "Revoke": the row's name tells them apart.
    expect(within(phone).getByRole('button', { name: 'Revoke' })).toHaveAccessibleDescription('Safari on iPhone');
    expect(within(phone).queryByRole('button', { name: 'Sign out' })).not.toBeInTheDocument();

    expect(within(laptop).getByText('Firefox on Linux')).toBeInTheDocument();
    expect(within(laptop).getByText('Last active 2 days ago')).toBeInTheDocument();
    expect(within(laptop).getByText('2001:db8::1')).toBeInTheDocument();

    expect(screen.getByRole('button', { name: 'Sign out other browsers' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Log out everywhere' })).toBeInTheDocument();
    // The account pages' links, with this one current.
    const nav = screen.getByRole('navigation', { name: 'Account' });
    expect(within(nav).getByRole('link', { name: 'Devices' })).toHaveAttribute('aria-current', 'page');
    expect(within(nav).getByRole('link', { name: 'Account' })).not.toHaveAttribute('aria-current');
  });

  it('leaves out what the server did not send', async () => {
    mockSessions([THIS, { id: 'n0n0n0n0n0n0', name: 'Browser', createdAt: THIS.createdAt, current: false }]);
    mockDevices();
    renderAccount('/account/devices');
    await ready();
    const [, bare] = await rows();
    if (!bare) throw new Error('expected two rows');
    expect(within(bare).getByText('Browser')).toBeInTheDocument();
    expect(within(bare).queryByText(/Last active/)).not.toBeInTheDocument();
    expect(within(bare).getByRole('button', { name: 'Revoke' })).toBeInTheDocument();
  });

  it('asks the server on every visit, whatever is cached', async () => {
    const list = mockSessions(ALL);
    const devices = mockDevices();
    const { services } = renderAccount('/account/devices');
    // A list from a moment ago, as an earlier visit left it.
    services.queryClient.setQueryData(queryKeys.meSessions, { sessions: [THIS] });
    services.queryClient.setQueryData(queryKeys.meDevices, { devices: [] });
    await ready();
    await waitFor(async () => {
      expect(await rows()).toHaveLength(3);
    });
    expect(list.gets.calls).toBe(1);
    expect(devices.calls).toBe(1);
  });

  it('revokes one: the row leaves, the others stay, and the heading takes the focus its button had', async () => {
    const list = mockSessions(ALL);
    mockDevices();
    const { services } = renderAccount('/account/devices');
    const user = await ready();
    const [, phone] = await rows();
    if (!phone) throw new Error('expected rows');
    await user.click(within(phone).getByRole('button', { name: 'Revoke' }));

    await waitFor(() => {
      expect(within(browsers()).queryByText('Safari on iPhone')).not.toBeInTheDocument();
    });
    expect(list.deleted).toEqual(['z9x8c7v6b5n4']);
    expect(names(await rows())).toEqual(['Chrome on macOS', 'Firefox on Linux']);
    expect(within(browsers()).getByRole('heading', { name: 'Browsers' })).toHaveFocus();
    expect(services.ui.getState().announcements.polite?.text).toBe('Signed out Safari on iPhone.');
    expect(services.ui.getState().toasts).toEqual([]);
    // The list comes from the server again.
    await waitFor(() => {
      expect(list.gets.calls).toBe(2);
    });
    expect(names(await rows())).toEqual(['Chrome on macOS', 'Firefox on Linux']);
  });

  it('not_found (404) on a revoke: it was signed out already, so the row leaves just the same', async () => {
    const list = mockSessions(ALL);
    // The session expired on the server after the list was loaded.
    const asked = onDelete('/api/v1/me/sessions/:id', (id) => {
      list.sessions = list.sessions.filter((s) => s.id !== id);
      return apiError(404, { code: 'not_found' });
    });
    mockDevices();
    const { services } = renderAccount('/account/devices');
    const user = await ready();
    const [, , laptop] = await rows();
    if (!laptop) throw new Error('expected rows');
    await user.click(within(laptop).getByRole('button', { name: 'Revoke' }));

    await waitFor(() => {
      expect(within(browsers()).queryByText('Firefox on Linux')).not.toBeInTheDocument();
    });
    expect(asked).toEqual(['p0o9i8u7y6t5']);
    expect(services.ui.getState().toasts).toEqual([]);
  });

  it.each([
    ['the server fails', () => apiError(500, { code: 'internal', requestId: 'req-7f3a' }), /Reference: req-7f3a$/],
    ['there is no connection', networkError, /You're offline\. Check your connection\.$/],
  ])('keeps the row and says why when %s', async (_name, respond, reason) => {
    mockSessions(ALL);
    onDelete('/api/v1/me/sessions/:id', respond);
    mockDevices();
    const { services } = renderAccount('/account/devices');
    const user = await ready();
    const [, phone] = await rows();
    if (!phone) throw new Error('expected rows');
    await user.click(within(phone).getByRole('button', { name: 'Revoke' }));

    await waitFor(() => {
      expect(services.ui.getState().toasts).toHaveLength(1);
    });
    const [toast] = services.ui.getState().toasts;
    expect(toast?.kind).toBe('error');
    expect(toast?.message).toMatch(/^Couldn't revoke Safari on iPhone\. /);
    expect(toast?.message).toMatch(reason);
    expect(names(await rows())).toEqual(['Chrome on macOS', 'Safari on iPhone', 'Firefox on Linux']);
    // It can be tried again.
    expect(within(phone).getByRole('button', { name: 'Revoke' })).not.toHaveAttribute('aria-busy');
  });

  it.each([
    [2, 'Signed out 2 other browsers.'],
    [1, 'Signed out 1 other browser.'],
    [0, 'No other browser was signed in.'],
  ])('"Sign out other browsers": %i revoked', async (revoked, message) => {
    const list = mockSessions(ALL);
    const posted = onPost('/api/v1/me/sessions/revoke-others', () => {
      list.sessions = [THIS];
      return HttpResponse.json({ revoked });
    });
    mockDevices();
    const { services, router } = renderAccount('/account/devices');
    const user = await ready();
    await rows();
    await user.click(screen.getByRole('button', { name: 'Sign out other browsers' }));

    await waitFor(() => {
      expect(services.ui.getState().toasts.map((toast) => [toast.kind, toast.message])).toEqual([['success', message]]);
    });
    expect(posted).toEqual([{}]);
    await waitFor(async () => {
      expect(names(await rows())).toEqual(['Chrome on macOS']);
    });
    // Nothing left to sign out: the button goes, and the heading takes the focus it had.
    expect(screen.queryByRole('button', { name: 'Sign out other browsers' })).not.toBeInTheDocument();
    await waitFor(() => {
      expect(within(browsers()).getByRole('heading', { name: 'Browsers' })).toHaveFocus();
    });
    // This browser stays signed in.
    expect(router.state.location.pathname).toBe('/account/devices');
    expect(services.queryClient.getQueryData(queryKeys.me)).not.toBeNull();
  });

  it('"Sign out other browsers" that fails: says why, and the list stays', async () => {
    mockSessions(ALL);
    onPost('/api/v1/me/sessions/revoke-others', () => apiError(503, { code: 'server_shutdown' }));
    mockDevices();
    const { services } = renderAccount('/account/devices');
    const user = await ready();
    await rows();
    await user.click(screen.getByRole('button', { name: 'Sign out other browsers' }));
    await waitFor(() => {
      expect(services.ui.getState().toasts.map((toast) => [toast.kind, toast.message])).toEqual([
        ['error', "Couldn't sign out the other browsers. The server is restarting."],
      ]);
    });
    expect(await rows()).toHaveLength(3);
    expect(screen.getByRole('button', { name: 'Sign out other browsers' })).not.toHaveAttribute('aria-busy');
  });

  it('has no "Sign out other browsers" when this is the only one', async () => {
    mockSessions([THIS]);
    mockDevices();
    renderAccount('/account/devices');
    await ready();
    expect(await rows()).toHaveLength(1);
    expect(screen.queryByRole('button', { name: 'Sign out other browsers' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Log out everywhere' })).toBeInTheDocument();
  });

  it("this browser's Sign out runs the logout flow and ends on the login page", async () => {
    mockSessions(ALL);
    mockDevices();
    const { session, router, services } = renderAccount('/account/devices');
    const loggedOut = onPost('/api/v1/auth/logout', () => {
      session.me = null;
      return noContent();
    });
    const user = await ready();
    const [mine] = await rows();
    if (!mine) throw new Error('expected rows');
    await user.click(within(mine).getByRole('button', { name: 'Sign out' }));

    expect(await screen.findByRole('heading', { name: LOGIN_HEADING })).toBeInTheDocument();
    expect(loggedOut).toEqual([{}]);
    expect(services.queryClient.getQueryData(queryKeys.me)).toBeNull();
    expect(router.state.location.pathname + router.state.location.search).toBe('/login?next=%2Faccount%2Fdevices');
  });

  it('says why the list could not be loaded, and loads it on "Try again"', async () => {
    let fail = true;
    const gets = onGet('/api/v1/me/sessions', () =>
      fail ? apiError(503, { code: 'server_shutdown' }) : HttpResponse.json({ sessions: ALL }),
    );
    mockDevices();
    renderAccount('/account/devices');
    const user = await ready();
    const alert = await within(browsers()).findByRole('alert');
    expect(alert).toHaveTextContent('The server is restarting.');
    expect(within(browsers()).queryByRole('listitem')).not.toBeInTheDocument();
    // What doesn't need the list still works.
    expect(screen.getByRole('button', { name: 'Log out everywhere' })).toBeInTheDocument();

    fail = false;
    await user.click(within(browsers()).getByRole('button', { name: 'Try again' }));
    expect(await rows()).toHaveLength(3);
    expect(within(browsers()).queryByRole('alert')).not.toBeInTheDocument();
    expect(gets.calls).toBe(2);
  });
});

describe('DevicesPage: log out everywhere', () => {
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
    await user.click(screen.getByRole('button', { name: 'Log out everywhere' }));
    const dialog = screen.getByRole('dialog', { name: 'Log out everywhere?' });
    expect(dialog).toHaveAttribute('open');
    return dialog;
  }

  it('asks first, and Cancel changes nothing', async () => {
    mockSessions(ALL);
    mockDevices();
    const posted = onPost('/api/v1/auth/logout-everywhere', noContent);
    const { services } = renderAccount('/account/devices');
    const user = await ready();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    const dialog = await openDialog(user);
    expect(
      within(dialog).getByText('This signs you out in every browser and every linked app, this one included.'),
    ).toBeInTheDocument();

    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(dialog).not.toHaveAttribute('open');
    expect(posted).toEqual([]);
    expect(services.queryClient.getQueryData(queryKeys.me)).not.toBeNull();
  });

  it('ends every session: this tab and the others are signed out, and the page goes to the login page', async () => {
    mockSessions(ALL);
    mockDevices();
    const { session, router, services } = renderAccount('/account/devices');
    const everywhere = onPost('/api/v1/auth/logout-everywhere', () => {
      session.me = null;
      return noContent();
    });
    // This tab's side of the sign-out is the logout flow, whose request has nothing left to end.
    const loggedOut = onPost('/api/v1/auth/logout', noContent);
    const user = await ready();
    const dialog = await openDialog(user);
    await user.click(within(dialog).getByRole('button', { name: 'Log out everywhere' }));

    expect(await screen.findByRole('heading', { name: LOGIN_HEADING })).toBeInTheDocument();
    expect(everywhere).toEqual([{}]);
    expect(loggedOut).toEqual([{}]);
    expect(services.queryClient.getQueryData(queryKeys.me)).toBeNull();
    // Nothing of the user stays in this tab's cache.
    expect(services.queryClient.getQueryData(queryKeys.meSessions)).toBeUndefined();
    await waitFor(() => {
      expect(heardByOtherTab).toEqual([{ type: 'logout' }]);
    });
    expect(router.state.location.pathname).toBe('/login');
    expect(services.ui.getState().toasts).toEqual([]);
  });

  it('still signs this tab and the others out when the logout flow cannot reach the server', async () => {
    mockSessions(ALL);
    mockDevices();
    const { session, services } = renderAccount('/account/devices');
    onPost('/api/v1/auth/logout-everywhere', () => {
      session.me = null;
      return noContent();
    });
    onPost('/api/v1/auth/logout', networkError);
    const user = await ready();
    await user.click(within(await openDialog(user)).getByRole('button', { name: 'Log out everywhere' }));

    expect(await screen.findByRole('heading', { name: LOGIN_HEADING })).toBeInTheDocument();
    expect(services.queryClient.getQueryData(queryKeys.me)).toBeNull();
    await waitFor(() => {
      expect(heardByOtherTab).toEqual([{ type: 'logout' }]);
    });
  });

  it.each([
    ['the server fails', () => apiError(500, { code: 'internal', requestId: 'req-7f3a' }), /Reference: req-7f3a/],
    ['there is no connection', networkError, /You're offline\. Check your connection\./],
  ])('stays signed in and says why in the dialog when %s', async (_name, respond, reason) => {
    mockSessions(ALL);
    mockDevices();
    let fail = true;
    const { session, services } = renderAccount('/account/devices');
    const posted = onPost('/api/v1/auth/logout-everywhere', () => {
      if (fail) return respond();
      session.me = null;
      return noContent();
    });
    onPost('/api/v1/auth/logout', noContent);
    const user = await ready();
    const dialog = await openDialog(user);
    await user.click(within(dialog).getByRole('button', { name: 'Log out everywhere' }));

    expect(await within(dialog).findByRole('alert')).toHaveTextContent(reason);
    expect(dialog).toHaveAttribute('open');
    expect(services.queryClient.getQueryData(queryKeys.me)).not.toBeNull();
    expect(heardByOtherTab).toEqual([]);

    // The same dialog can try again.
    fail = false;
    await user.click(within(dialog).getByRole('button', { name: 'Log out everywhere' }));
    expect(await screen.findByRole('heading', { name: LOGIN_HEADING })).toBeInTheDocument();
    expect(posted).toHaveLength(2);
  });
});

describe('DevicesPage: linked devices', () => {
  function linked(): HTMLElement {
    return screen.getByRole('region', { name: 'Linked devices' });
  }

  it('is empty in M1: says that nothing is linked, with nothing to revoke', async () => {
    mockSessions([THIS]);
    const devices = mockDevices();
    renderAccount('/account/devices');
    await ready();
    expect(
      await within(linked()).findByText(
        'Nothing is linked yet. Apps you sign in to with this account, like the isshoni desktop app when it arrives, show up here.',
      ),
    ).toBeInTheDocument();
    expect(devices.calls).toBe(1);
    expect(within(linked()).queryByRole('listitem')).not.toBeInTheDocument();
    expect(within(linked()).queryByRole('button')).not.toBeInTheDocument();
  });

  it('lists a linked device and revokes it', async () => {
    mockSessions([THIS]);
    let devices = [
      deviceFixture({ lastSeenAt: new Date(Date.now() - 2 * 60 * MINUTE).toISOString() }),
      deviceFixture({ id: 'e1e2e3e4e5e6', name: 'Alex-Laptop', lastIp: '198.51.100.23' }),
    ];
    const gets = onGet('/api/v1/me/devices', () => HttpResponse.json({ devices }));
    const deleted = onDelete('/api/v1/me/devices/:id', (id) => {
      devices = devices.filter((d) => d.id !== id);
      return noContent();
    });
    const { services } = renderAccount('/account/devices');
    const user = await ready();
    const [pc, laptop] = await within(linked()).findAllByRole('listitem');
    if (!pc || !laptop) throw new Error('expected two rows');
    expect(within(pc).getByText('Alex-PC')).toBeInTheDocument();
    expect(within(pc).getByText('Last active 2 hours ago')).toBeInTheDocument();
    expect(within(pc).getByText('203.0.113.7')).toBeInTheDocument();
    expect(within(pc).getByRole('button', { name: 'Revoke' })).toHaveAccessibleDescription('Alex-PC');
    expect(within(linked()).queryByText(/Nothing is linked yet/)).not.toBeInTheDocument();

    await user.click(within(pc).getByRole('button', { name: 'Revoke' }));
    await waitFor(() => {
      expect(within(linked()).queryByText('Alex-PC')).not.toBeInTheDocument();
    });
    expect(deleted).toEqual(['d1d2d3d4d5d6']);
    expect(within(linked()).getByText('Alex-Laptop')).toBeInTheDocument();
    expect(within(linked()).getByRole('heading', { name: 'Linked devices' })).toHaveFocus();
    expect(services.ui.getState().announcements.polite?.text).toBe('Signed out Alex-PC.');
    await waitFor(() => {
      expect(gets.calls).toBe(2);
    });
  });

  it('says why the list could not be loaded, without taking the browsers list with it', async () => {
    mockSessions(ALL);
    onGet('/api/v1/me/devices', () => apiError(500, { code: 'internal', requestId: 'req-7f3a' }));
    renderAccount('/account/devices');
    await ready();
    expect(await within(linked()).findByRole('alert')).toHaveTextContent(
      'Something went wrong on the server. Reference: req-7f3a',
    );
    expect(within(linked()).queryByText(/Nothing is linked yet/)).not.toBeInTheDocument();
    expect(await rows()).toHaveLength(3);
  });
});
