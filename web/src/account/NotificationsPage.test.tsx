// /account/notifications (05 §16.3) with a fake NotificationsProvider and MSW: the switch and the test notification
// where push works, the help for every other support state, the in-app browser banner, Install app, and the
// preferences (03 §12.4.6, GET and PUT /api/v1/push/preferences).
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';

import { clientOS, inAppBrowser } from '../lib/ua';
import type { Platform } from '../platform/types';
import type { Info, PushPreferences } from '../protocol/api.gen';
import { ApiError, NetworkError } from '../protocol/rest';
import { apiError, apiPath, meFixture, server } from '../test/msw';
import { createTestPlatform } from '../test/platform';
import { pushStore } from './pushState';
import { renderAccount } from './testing/harness';
import { fakePush, fakePwa, UA, type FakePush } from './testing/push';

const HEADING = 'Notifications';
const SWITCH = 'Notifications on this device';
const WITH_PUSH: Partial<Info> = { push: { vapidPublicKey: 'BNcRdreALRFXTkOOUHK1EtK2wtaz5Ry4YfYCA_0QTpQ' } };

function platform(push: FakePush, ua: string = UA.chromeMac, extra: Partial<Platform> = {}): Platform {
  return createTestPlatform({
    client: { kind: 'web', version: '0.0.0-test', os: clientOS(ua), browser: 'other' },
    inAppBrowser: () => inAppBrowser(ua),
    notifications: push,
    ...extra,
  });
}

/** GET and PUT /api/v1/push/preferences over one stored value; PUT answers what it stored. */
function mockPreferences(initial: PushPreferences = { shareStarted: 'all', adminAlerts: true }) {
  const state = { prefs: initial, puts: [] as unknown[] };
  server.use(
    http.get(apiPath('/api/v1/push/preferences'), () => HttpResponse.json(state.prefs)),
    http.put(apiPath('/api/v1/push/preferences'), async ({ request }) => {
      const body = (await request.json()) as PushPreferences;
      state.puts.push(body);
      state.prefs = body;
      return HttpResponse.json(body);
    }),
  );
  return state;
}

function open(
  push: FakePush,
  opts: { ua?: string; info?: Partial<Info>; admin?: boolean; extra?: Partial<Platform> } = {},
) {
  const r = renderAccount('/account/notifications', {
    platform: platform(push, opts.ua, opts.extra),
    info: opts.info ?? WITH_PUSH,
    me: meFixture({ admin: opts.admin ?? false }),
  });
  const toasts = (): string[] => r.services.ui.getState().toasts.map((t) => t.message);
  return { ...r, toasts };
}

const device = () => screen.getByRole('region', { name: 'This device' });

describe('NotificationsPage: this device (05 §16.3)', () => {
  it('turns notifications on from the switch, then offers a test notification', async () => {
    mockPreferences();
    const push = fakePush('supported');
    const page = open(push);
    expect(await screen.findByRole('heading', { level: 1, name: HEADING })).toBeInTheDocument();
    const toggle = await screen.findByRole('switch', { name: SWITCH });
    expect(toggle).not.toBeChecked();
    expect(device()).toHaveTextContent('Off: this device gets no notifications.');
    expect(screen.queryByRole('button', { name: 'Send a test notification' })).not.toBeInTheDocument();
    // Never by itself: the page asks the browser only on the click.
    expect(push.enable).not.toHaveBeenCalled();

    const user = userEvent.setup();
    await user.click(toggle);
    expect(push.enable).toHaveBeenCalledTimes(1);
    await waitFor(() => {
      expect(toggle).toBeChecked();
    });
    expect(page.toasts()).toContain('Notifications are on for this device.');

    await user.click(screen.getByRole('button', { name: 'Send a test notification' }));
    expect(push.test).toHaveBeenCalledTimes(1);
    await waitFor(() => {
      expect(page.toasts()).toContain('Test sent. It should arrive within a few seconds.');
    });
  });

  it('turns them off again', async () => {
    mockPreferences();
    const push = fakePush('supported', 'on');
    open(push);
    const toggle = await screen.findByRole('switch', { name: SWITCH });
    await waitFor(() => {
      expect(toggle).toBeChecked();
    });
    await userEvent.setup().click(toggle);
    expect(push.disable).toHaveBeenCalledTimes(1);
    await waitFor(() => {
      expect(toggle).not.toBeChecked();
    });
  });

  it.each([
    ['the server’s reason', new ApiError({ status: 503, code: 'push_unavailable' }), 'Notifications are turned off'],
    ['no network', new NetworkError(), "You're offline."],
    ['the browser’s own failure', new Error('push: subscribe_failed'), "This browser couldn't turn on notifications."],
  ])('says why turning on failed: %s', async (_name, err, text) => {
    mockPreferences();
    const push = fakePush('supported');
    push.enable.mockRejectedValue(err);
    open(push);
    await userEvent.setup().click(await screen.findByRole('switch', { name: SWITCH }));
    expect(await within(device()).findByRole('alert')).toHaveTextContent(text);
    expect(screen.getByRole('switch', { name: SWITCH })).not.toBeChecked();
  });

  it('shows only its own failures, not one from elsewhere on the page earlier (the room’s card)', async () => {
    mockPreferences();
    const push = fakePush('supported');
    const p = platform(push);
    pushStore(p).setState({ error: new NetworkError() });
    renderAccount('/account/notifications', { platform: p, info: WITH_PUSH });
    await screen.findByRole('switch', { name: SWITCH });
    expect(within(device()).queryByRole('alert')).not.toBeInTheDocument();
  });

  it('says how long to wait when the test is asked for too often', async () => {
    mockPreferences();
    const push = fakePush('supported', 'on');
    push.test.mockRejectedValue(new ApiError({ status: 429, code: 'rate_limited', retryAfterSec: 7 }));
    open(push);
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Send a test notification' }));
    expect(await within(device()).findByRole('alert')).toHaveTextContent('7');
  });

  it('asks the browser again on every visit: a permission changed in its settings shows', async () => {
    mockPreferences();
    const push = fakePush('supported');
    const first = open(push);
    await screen.findByRole('switch', { name: SWITCH });
    first.unmount();
    push.state.support = 'denied';
    renderAccount('/account/notifications', { platform: first.services.platform, info: WITH_PUSH });
    expect(await screen.findByText(/Notifications are blocked for isshoni/)).toBeInTheDocument();
  });

  it('explains how to allow them again after a block', async () => {
    mockPreferences();
    open(fakePush('denied'));
    expect(await screen.findByText(/allow notifications for this site in the browser's site settings/)).toBeVisible();
    expect(screen.queryByRole('switch')).not.toBeInTheDocument();
  });

  it.each([
    [
      'a server without push',
      fakePush('unsupported'),
      UA.chromeMac,
      {},
      'Notifications are turned off on this server.',
    ],
    [
      'an iOS without the Push API',
      fakePush('unsupported'),
      UA.safariIPhone,
      WITH_PUSH,
      'Update to iOS 16.4 or later.',
    ],
    ['another browser', fakePush('unsupported'), UA.chromeMac, WITH_PUSH, "This browser can't show isshoni"],
  ])('says why it can’t: %s', async (_name, push, ua, info, text) => {
    mockPreferences();
    open(push, { ua, info: { push: undefined, ...info } });
    const region = await screen.findByRole('region', { name: 'This device' });
    expect(await within(region).findByText(text, { exact: false })).toBeVisible();
    expect(screen.queryByRole('switch')).not.toBeInTheDocument();
  });

  it('gives the Home Screen steps on iOS in a Safari tab', async () => {
    mockPreferences();
    open(fakePush('needs-install'), { ua: UA.safariIPhone });
    const region = await screen.findByRole('region', { name: 'This device' });
    expect(await within(region).findByText(/Add isshoni to your Home Screen to get notifications/)).toBeVisible();
    expect(within(region).getByRole('list', { name: 'How to add it' })).toHaveTextContent('Choose Add to Home Screen.');
    expect(region).toHaveTextContent("You'll log in once more inside the app");
    expect(region).toHaveTextContent('Use its ⋯ menu → Open in Safari first.');
  });

  it('in a chat app’s browser shows the in-app banner first, and the steps once it is dismissed', async () => {
    mockPreferences();
    open(fakePush('needs-install'), { ua: UA.instagramIPhone });
    const banner = await screen.findByRole('region', { name: 'Open in your browser' });
    await screen.findByRole('region', { name: 'This device' });
    expect(screen.queryByText(/Add isshoni to your Home Screen/)).not.toBeInTheDocument();
    await userEvent.setup().click(within(banner).getByRole('button', { name: 'Dismiss' }));
    expect(await screen.findByText(/Add isshoni to your Home Screen to get notifications/)).toBeVisible();
  });

  it('offers Install app while the browser has an install prompt', async () => {
    mockPreferences();
    const pwa = fakePwa('prompt');
    open(fakePush('supported'), { extra: { pwa } });
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Install app' }));
    expect(pwa.promptInstall).toHaveBeenCalledTimes(1);
  });
});

describe('NotificationsPage: what to tell me about (03 §12.4.6)', () => {
  it('shows a member’s choice and saves a change with both fields', async () => {
    const prefs = mockPreferences({ shareStarted: 'all', adminAlerts: true });
    open(fakePush('supported'));
    const share = await screen.findByRole('checkbox', { name: 'When someone starts sharing' });
    expect(share).toBeChecked();
    expect(screen.queryByRole('checkbox', { name: 'Admin alerts' })).not.toBeInTheDocument();
    await userEvent.setup().click(share);
    await waitFor(() => {
      expect(prefs.puts).toEqual([{ shareStarted: 'off', adminAlerts: true }]);
    });
    await waitFor(() => {
      expect(share).not.toBeChecked();
    });
  });

  it('gives admins the admin alerts', async () => {
    const prefs = mockPreferences({ shareStarted: 'off', adminAlerts: true });
    open(fakePush('supported'), { admin: true });
    const alerts = await screen.findByRole('checkbox', { name: 'Admin alerts' });
    expect(alerts).toBeChecked();
    expect(screen.getByRole('checkbox', { name: 'When someone starts sharing' })).not.toBeChecked();
    await userEvent.setup().click(alerts);
    await waitFor(() => {
      expect(prefs.puts).toEqual([{ shareStarted: 'off', adminAlerts: false }]);
    });
  });

  it('puts the choice back and says so when saving failed', async () => {
    mockPreferences({ shareStarted: 'all', adminAlerts: true });
    server.use(http.put(apiPath('/api/v1/push/preferences'), () => apiError(503, { code: 'server_busy' })));
    open(fakePush('supported'));
    const share = await screen.findByRole('checkbox', { name: 'When someone starts sharing' });
    await userEvent.setup().click(share);
    expect(await screen.findByText(/Couldn't save that\./)).toBeVisible();
    expect(share).toBeChecked();
  });

  it('offers to try again when the choices can’t be loaded', async () => {
    let fail = true;
    server.use(
      http.get(apiPath('/api/v1/push/preferences'), () =>
        fail ? apiError(503, { code: 'server_busy' }) : HttpResponse.json({ shareStarted: 'all', adminAlerts: true }),
      ),
    );
    open(fakePush('supported'));
    expect(await screen.findByText(/Couldn't load your choices\./)).toBeVisible();
    fail = false;
    await userEvent.setup().click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('checkbox', { name: 'When someone starts sharing' })).toBeChecked();
  });

  it('is not there while the server has no push', async () => {
    open(fakePush('unsupported'), { info: { push: undefined } });
    await screen.findByText('Notifications are turned off on this server.');
    expect(screen.queryByRole('region', { name: 'What to tell me about' })).not.toBeInTheDocument();
  });
});
