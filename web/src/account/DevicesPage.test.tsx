// /account/devices (05 §15.2) against MSW: the list of browser sessions (03 §12.3 #14), revoking one (#15), "Sign
// out other browsers" (#16), "Log out everywhere" (#4), this browser's Sign out, and the linked apps (#17, #18 and
// M2's #52, M2 02 §10.2): each app's row, Rename, Remove with its confirmation, the "This app" badge only when
// `current` is true, and the cookie-only action left out for a bearer.
import { screen, waitFor, within } from '@testing-library/react';
import userEvent, { type UserEvent } from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { CHANNEL_NAME } from '../app/session';
import { networkError, noContent, onPost } from '../auth/testing/harness';
import { formatDateTime } from '../lib/time';
import type { DeviceInfo, Me, SessionInfo } from '../protocol/api.gen';
import { queryKeys } from '../protocol/queryKeys';
import { apiError, apiPath, meFixture, server } from '../test/msw';
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

describe('DevicesPage: linked apps (M2 02 §10.2)', () => {
  function linked(): HTMLElement {
    return screen.getByRole('region', { name: 'Linked devices' });
  }

  async function appRows(): Promise<HTMLElement[]> {
    return within(linked()).findAllByRole('listitem');
  }

  /** A server-side list of apps that GET, PATCH and DELETE act on, as the real endpoints do. */
  interface AppList {
    devices: DeviceInfo[];
    /** How often GET /api/v1/me/devices was asked. */
    readonly gets: { calls: number };
    /** The ids DELETE /api/v1/me/devices/{id} was asked for. */
    readonly deleted: string[];
    /** The PATCH /api/v1/me/devices/{id} requests. */
    readonly renamed: { id: string; body: unknown }[];
  }

  function mockApps(initial: DeviceInfo[]): AppList {
    const state: AppList = {
      devices: initial,
      gets: onGet('/api/v1/me/devices', () => HttpResponse.json({ devices: state.devices })),
      deleted: onDelete('/api/v1/me/devices/:id', (id) => {
        state.devices = state.devices.filter((d) => d.id !== id);
        return noContent();
      }),
      renamed: onPatch('/api/v1/me/devices/:id', (id, body) => {
        const name = (body as { name: string }).name;
        state.devices = state.devices.map((d) => (d.id === id ? { ...d, name } : d));
        const device = state.devices.find((d) => d.id === id);
        return device ? HttpResponse.json({ device }) : apiError(404, { code: 'not_found' });
      }),
    };
    return state;
  }

  let PC: DeviceInfo;
  let LAPTOP: DeviceInfo;

  beforeEach(() => {
    PC = deviceFixture({ lastSeenAt: new Date(Date.now() - 2 * 60 * MINUTE).toISOString() });
    LAPTOP = deviceFixture({
      id: 'e1e2e3e4e5e6',
      name: 'Alex-Laptop',
      os: 'macos',
      appVersion: '0.3.0',
      lastIp: '198.51.100.23',
    });
  });

  it('is empty without a linked app: says so, with nothing to remove', async () => {
    mockSessions([THIS]);
    const devices = mockDevices();
    renderAccount('/account/devices');
    await ready();
    expect(
      await within(linked()).findByText(
        'Nothing is linked yet. When you link the isshoni app to this account, it shows up here.',
      ),
    ).toBeInTheDocument();
    expect(devices.calls).toBe(1);
    expect(within(linked()).queryByRole('listitem')).not.toBeInTheDocument();
    expect(within(linked()).queryByRole('button')).not.toBeInTheDocument();
  });

  it('lists each app: its name, the app and version, when it was linked, and when and where it was last seen', async () => {
    mockSessions([THIS]);
    mockApps([PC, LAPTOP]);
    renderAccount('/account/devices');
    await ready();
    const [pc, laptop, ...rest] = await appRows();
    expect(rest).toEqual([]);
    if (!pc || !laptop) throw new Error('expected two rows');

    expect(within(pc).getByText('Alex-PC')).toBeInTheDocument();
    expect(within(pc).getByText('isshoni for Windows 0.2.0')).toBeInTheDocument();
    const linkedOn = within(pc).getByText(/^Linked /);
    expect(linkedOn.tagName).toBe('TIME');
    expect(linkedOn).toHaveTextContent(
      `Linked ${new Intl.DateTimeFormat('en', { dateStyle: 'medium' }).format(new Date(PC.createdAt ?? ''))}`,
    );
    expect(linkedOn).toHaveAttribute('title', formatDateTime(new Date(PC.createdAt ?? ''), 'en'));
    const seen = within(pc).getByText('Last seen 2 hours ago from 203.0.113.7');
    expect(seen.tagName).toBe('TIME');
    expect(seen).toHaveAttribute('datetime', PC.lastSeenAt);
    // Every row's buttons say the same: the row's name tells them apart.
    expect(within(pc).getByRole('button', { name: 'Rename' })).toHaveAccessibleDescription('Alex-PC');
    expect(within(pc).getByRole('button', { name: 'Remove' })).toHaveAccessibleDescription('Alex-PC');
    // A cookie session's list: no app is this one.
    expect(within(pc).queryByText('This app')).not.toBeInTheDocument();

    expect(within(laptop).getByText('Alex-Laptop')).toBeInTheDocument();
    expect(within(laptop).getByText('isshoni for macOS 0.3.0')).toBeInTheDocument();
    expect(within(laptop).getByText('Last seen now from 198.51.100.23')).toBeInTheDocument();
    expect(within(linked()).queryByText(/Nothing is linked yet/)).not.toBeInTheDocument();
  });

  it('leaves out what the server did not send, and names an app it doesn’t know', async () => {
    mockSessions([THIS]);
    mockApps([
      {
        id: 'u1u2u3u4u5u6',
        name: 'Something new',
        clientKind: 'agent',
        current: false,
      },
      deviceFixture({ id: 'p1p2p3p4p5p6', name: 'Phone', clientKind: 'mobile', os: 'ios', lastIp: '' }),
    ]);
    renderAccount('/account/devices');
    await ready();
    const [bare, phone] = await appRows();
    if (!bare || !phone) throw new Error('expected two rows');
    expect(within(bare).getByText('An isshoni app')).toBeInTheDocument();
    expect(within(bare).queryByText(/^Linked /)).not.toBeInTheDocument();
    expect(within(bare).queryByText(/^Last seen/)).not.toBeInTheDocument();
    expect(within(phone).getByText('isshoni for iPhone 0.2.0')).toBeInTheDocument();
    expect(within(phone).getByText('Last seen now')).toBeInTheDocument();
  });

  it('shows "This app" and its own Remove wording only for the caller’s own app (current: a bearer)', async () => {
    mockSessions([THIS]);
    mockApps([{ ...PC, current: true }, LAPTOP]);
    renderAccount('/account/devices');
    const user = await ready();
    const [mine, laptop] = await appRows();
    if (!mine || !laptop) throw new Error('expected two rows');
    expect(within(mine).getByText('This app')).toBeInTheDocument();
    expect(within(laptop).queryByText('This app')).not.toBeInTheDocument();

    await user.click(within(mine).getByRole('button', { name: 'Remove' }));
    const dialog = screen.getByRole('dialog', { name: 'Remove Alex-PC?' });
    expect(within(dialog).getByText('This app will be signed out.')).toBeInTheDocument();
    expect(within(dialog).queryByText(/has to be linked again/)).not.toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));

    await user.click(within(laptop).getByRole('button', { name: 'Remove' }));
    expect(
      within(screen.getByRole('dialog', { name: 'Remove Alex-Laptop?' })).getByText(
        'Alex-Laptop will be signed out and has to be linked again.',
      ),
    ).toBeInTheDocument();
  });

  it('removes an app after a confirmation: Cancel changes nothing, Remove signs it out and its row goes', async () => {
    mockSessions([THIS]);
    const apps = mockApps([PC, LAPTOP]);
    const { services } = renderAccount('/account/devices');
    const user = await ready();
    const [pc] = await appRows();
    if (!pc) throw new Error('expected rows');

    await user.click(within(pc).getByRole('button', { name: 'Remove' }));
    let dialog = screen.getByRole('dialog', { name: 'Remove Alex-PC?' });
    expect(dialog).toHaveAttribute('open');
    expect(within(dialog).getByText('Alex-PC will be signed out and has to be linked again.')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(dialog).not.toHaveAttribute('open');
    expect(apps.deleted).toEqual([]);

    await user.click(within(pc).getByRole('button', { name: 'Remove' }));
    dialog = screen.getByRole('dialog', { name: 'Remove Alex-PC?' });
    await user.click(within(dialog).getByRole('button', { name: 'Remove' }));
    await waitFor(() => {
      expect(within(linked()).queryByText('Alex-PC')).not.toBeInTheDocument();
    });
    expect(apps.deleted).toEqual(['d1d2d3d4d5d6']);
    expect(within(linked()).getByText('Alex-Laptop')).toBeInTheDocument();
    expect(within(linked()).getByRole('heading', { name: 'Linked devices' })).toHaveFocus();
    expect(services.ui.getState().announcements.polite?.text).toBe('Removed Alex-PC.');
    await waitFor(() => {
      expect(apps.gets.calls).toBe(2);
    });
  });

  it('not_found (404) on a remove: it was removed already, so the row goes just the same', async () => {
    mockSessions([THIS]);
    let devices = [PC];
    onGet('/api/v1/me/devices', () => HttpResponse.json({ devices }));
    // The app was removed on the server after the list was loaded (another tab, or it signed itself out).
    const asked = onDelete('/api/v1/me/devices/:id', (id) => {
      devices = devices.filter((d) => d.id !== id);
      return apiError(404, { code: 'not_found' });
    });
    const { services } = renderAccount('/account/devices');
    const user = await ready();
    const [pc] = await appRows();
    if (!pc) throw new Error('expected rows');
    await user.click(within(pc).getByRole('button', { name: 'Remove' }));
    await user.click(
      within(screen.getByRole('dialog', { name: 'Remove Alex-PC?' })).getByRole('button', { name: 'Remove' }),
    );
    await waitFor(() => {
      expect(within(linked()).queryByText('Alex-PC')).not.toBeInTheDocument();
    });
    expect(asked).toEqual(['d1d2d3d4d5d6']);
    expect(services.ui.getState().toasts).toEqual([]);
  });

  it.each([
    ['the server fails', () => apiError(500, { code: 'internal', requestId: 'req-7f3a' }), /Reference: req-7f3a/],
    ['there is no connection', networkError, /You're offline\. Check your connection\./],
  ])('keeps the row and says why in the dialog when %s; the dialog can try again', async (_name, respond, reason) => {
    mockSessions([THIS]);
    let devices = [PC];
    onGet('/api/v1/me/devices', () => HttpResponse.json({ devices }));
    let fail = true;
    const asked = onDelete('/api/v1/me/devices/:id', (id) => {
      if (fail) return respond();
      devices = devices.filter((d) => d.id !== id);
      return noContent();
    });
    renderAccount('/account/devices');
    const user = await ready();
    const [pc] = await appRows();
    if (!pc) throw new Error('expected rows');
    await user.click(within(pc).getByRole('button', { name: 'Remove' }));
    const dialog = screen.getByRole('dialog', { name: 'Remove Alex-PC?' });
    await user.click(within(dialog).getByRole('button', { name: 'Remove' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(reason);
    expect(dialog).toHaveAttribute('open');
    expect(within(linked()).getByText('Alex-PC')).toBeInTheDocument();

    fail = false;
    await user.click(within(dialog).getByRole('button', { name: 'Remove' }));
    await waitFor(() => {
      expect(within(linked()).queryByText('Alex-PC')).not.toBeInTheDocument();
    });
    expect(asked).toHaveLength(2);
  });

  it('renames inline: the trimmed name goes to the server, the row shows it, and focus returns to Rename', async () => {
    mockSessions([THIS]);
    const apps = mockApps([PC, LAPTOP]);
    const { services } = renderAccount('/account/devices');
    const user = await ready();
    const [pc] = await appRows();
    if (!pc) throw new Error('expected rows');

    await user.click(within(pc).getByRole('button', { name: 'Rename' }));
    const field = within(pc).getByLabelText('New name for Alex-PC');
    expect(field).toHaveFocus();
    expect(field).toHaveValue('Alex-PC');
    // The row's buttons make way for Save and Cancel.
    expect(within(pc).queryByRole('button', { name: 'Remove' })).not.toBeInTheDocument();
    await user.clear(field);
    await user.type(field, '  Gaming PC  {Enter}');

    await waitFor(() => {
      expect(within(pc).getByText('Gaming PC')).toBeInTheDocument();
    });
    expect(apps.renamed).toEqual([{ id: 'd1d2d3d4d5d6', body: { name: 'Gaming PC' } }]);
    expect(within(pc).queryByRole('textbox')).not.toBeInTheDocument();
    expect(within(pc).getByRole('button', { name: 'Rename' })).toHaveFocus();
    expect(within(pc).getByRole('button', { name: 'Rename' })).toHaveAccessibleDescription('Gaming PC');
    expect(services.ui.getState().announcements.polite?.text).toBe('Renamed to Gaming PC.');
  });

  it('closes the rename field without a request on Esc, on Cancel and for an unchanged name', async () => {
    mockSessions([THIS]);
    const apps = mockApps([PC]);
    renderAccount('/account/devices');
    const user = await ready();
    const [pc] = await appRows();
    if (!pc) throw new Error('expected rows');

    await user.click(within(pc).getByRole('button', { name: 'Rename' }));
    await user.type(within(pc).getByLabelText('New name for Alex-PC'), 'X{Escape}');
    expect(within(pc).queryByRole('textbox')).not.toBeInTheDocument();
    expect(within(pc).getByText('Alex-PC')).toBeInTheDocument();
    expect(within(pc).getByRole('button', { name: 'Rename' })).toHaveFocus();

    await user.click(within(pc).getByRole('button', { name: 'Rename' }));
    await user.click(within(pc).getByRole('button', { name: 'Cancel' }));
    expect(within(pc).queryByRole('textbox')).not.toBeInTheDocument();

    await user.click(within(pc).getByRole('button', { name: 'Rename' }));
    const field = within(pc).getByLabelText('New name for Alex-PC');
    await user.clear(field);
    await user.type(field, ' Alex-PC {Enter}');
    expect(within(pc).queryByRole('textbox')).not.toBeInTheDocument();
    expect(apps.renamed).toEqual([]);
  });

  it.each([
    ['an empty name, before anything is sent', '   ', null, 'Enter a name.'],
    [
      'a name over 64 characters, before anything is sent',
      'x'.repeat(65),
      null,
      "That's too long for a name: 64 characters at most.",
    ],
    ['422 {name: invalid}', 'Bad​Name', 'invalid', "That name has characters that can't be used."],
    ['422 with a code without a text of its own', 'Odd', 'not_allowed', 'Check the highlighted fields.'],
  ])('shows the problem under the field: %s', async (_name, typed, serverCode, shown) => {
    mockSessions([THIS]);
    mockApps([PC]);
    const sent: unknown[] = [];
    onPatch('/api/v1/me/devices/:id', (_id, body) => {
      sent.push(body);
      return apiError(422, { code: 'validation_failed', fields: { name: serverCode ?? 'invalid' } });
    });
    renderAccount('/account/devices');
    const user = await ready();
    const [pc] = await appRows();
    if (!pc) throw new Error('expected rows');
    await user.click(within(pc).getByRole('button', { name: 'Rename' }));
    const field = within(pc).getByLabelText('New name for Alex-PC');
    await user.clear(field);
    await user.type(field, `${typed}{Enter}`);

    await waitFor(() => {
      expect(field).toHaveAccessibleDescription(shown);
    });
    expect(field).toHaveAttribute('aria-invalid', 'true');
    expect(field).toHaveFocus();
    expect(sent).toHaveLength(serverCode === null ? 0 : 1);
  });

  it('says why a rename failed under the field (a server error), and a 404 takes the row away', async () => {
    mockSessions([THIS]);
    // The laptop was removed on the server after the list was loaded.
    let devices = [PC, LAPTOP];
    const gets = onGet('/api/v1/me/devices', () => HttpResponse.json({ devices }));
    onPatch('/api/v1/me/devices/:id', (id) => {
      if (id === PC.id) return apiError(500, { code: 'internal', requestId: 'req-7f3a' });
      devices = devices.filter((d) => d.id !== id);
      return apiError(404, { code: 'not_found' });
    });
    const { services } = renderAccount('/account/devices');
    const user = await ready();
    const [pc, laptop] = await appRows();
    if (!pc || !laptop) throw new Error('expected rows');

    await user.click(within(pc).getByRole('button', { name: 'Rename' }));
    await user.type(within(pc).getByLabelText('New name for Alex-PC'), '2{Enter}');
    await waitFor(() => {
      expect(within(pc).getByLabelText('New name for Alex-PC')).toHaveAccessibleDescription(
        'Something went wrong on the server. Reference: req-7f3a',
      );
    });

    await user.click(within(laptop).getByRole('button', { name: 'Rename' }));
    await user.type(within(laptop).getByLabelText('New name for Alex-Laptop'), '2{Enter}');
    await waitFor(() => {
      expect(within(linked()).queryByText('Alex-Laptop')).not.toBeInTheDocument();
    });
    expect(services.ui.getState().announcements.polite?.text).toBe("Alex-Laptop isn't linked anymore.");
    // The list comes from the server again, without it.
    await waitFor(() => {
      expect(gets.calls).toBe(2);
    });
    expect(within(linked()).queryByText('Alex-Laptop')).not.toBeInTheDocument();
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

describe('DevicesPage for a bearer (02 §10.2: room for later clients)', () => {
  it('leaves out "Sign out other browsers", which is cookie-only; Log out everywhere stays', async () => {
    // GET /me for a bearer: {device} instead of {session} (02 §7.2).
    const bearer: Me = { ...meFixture(), device: deviceFixture({ current: true }) };
    delete bearer.session;
    mockSessions(ALL.map((s) => ({ ...s, current: false })));
    mockDevices([deviceFixture({ current: true })]);
    renderAccount('/account/devices', { me: bearer });
    await ready();
    expect(await rows()).toHaveLength(3);
    expect(screen.queryByRole('button', { name: 'Sign out other browsers' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Log out everywhere' })).toBeInTheDocument();
    // No browser is this one for a bearer: each has Revoke, none Sign out.
    expect(within(browsers()).queryByRole('button', { name: 'Sign out' })).not.toBeInTheDocument();
    expect(within(screen.getByRole('region', { name: 'Linked devices' })).getByText('This app')).toBeInTheDocument();
  });

  it('a cookie session keeps "Sign out other browsers"', async () => {
    mockSessions(ALL);
    mockDevices();
    renderAccount('/account/devices');
    await ready();
    await rows();
    expect(screen.getByRole('button', { name: 'Sign out other browsers' })).toBeInTheDocument();
  });
});

/**
 * Installs a PATCH handler for a path with one `:id` segment that records each id and JSON body and answers with
 * respond(id, body). Returns the recorded calls.
 */
function onPatch(
  path: `/api/${string}`,
  respond: (id: string, body: unknown) => Response | Promise<Response>,
): { id: string; body: unknown }[] {
  const calls: { id: string; body: unknown }[] = [];
  server.use(
    http.patch(apiPath(path), async ({ params, request }) => {
      const id = String(params['id']);
      const body: unknown = await request.json();
      calls.push({ id, body });
      return respond(id, body);
    }),
  );
  return calls;
}
