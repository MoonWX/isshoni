// The Settings page (docs/m2/03 §14.4, §22.3 "Settings"; README WA82): one describe per section (Servers, Sharing,
// App, Updates, About; Audio is AudioSettings.test.tsx), the update states of §16.2 (the required banner, expired,
// failed, the beta channel, policyOff and the others) and the anchors. The bridge is the fake NativeBridge: every call
// is recorded, and Settings.Update applies its patch to the view it answers, as Go does.
import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createDesktopTestPlatform, type DesktopTestPlatform } from '../app/testing/desktop';
import { Toasts } from '../app/Toasts';
import { i18n } from '../i18n';
import type {
  HotkeyStatus,
  ServerInfo,
  SettingsPatch,
  SettingsView,
  ShareCaps,
  UpdateStatus,
} from '../platform/desktop/bridge';
import * as codes from '../platform/desktop/codes.gen';
import { BridgeError } from '../platform/desktop/errors';
import { shareView } from '../platform/desktop/testing/shareView';
import { updateStatus } from '../platform/desktop/testing/fakeBridge';
import { friendsHear } from '../share/native/testing';
import { testOrigin } from '../test/platform';
import { CODE_SIGNING_URL, PRIVACY_DESKTOP_URL, versionInfoText, webViewVersion } from './AboutSettings';
import { OPENH264_LICENSE_URL, openH264ErrorText, type OpenH264Control, type OpenH264Status } from './OpenH264OptIn';
import { renderDesktopPage } from './testing/render';
import { RequiredUpdateBanner, UPDATES_HELP } from './UpdateParts';

function settingsView(over: Partial<SettingsView> = {}): SettingsView {
  return {
    schema: 1,
    serverPrefs: {},
    wizardDone: true,
    window: { x: 0, y: 0, w: 1200, h: 760, maximized: false },
    closeToTray: false,
    autostart: false,
    hideFromCapture: true,
    notifications: true,
    hotkeys: { pause: 'Ctrl+Alt+Shift+F9', stop: 'Ctrl+Alt+Shift+F10' },
    pill: { mode: 'auto', positions: {} },
    updates: { check: true, channel: 'stable', policyOff: false },
    tips: { firstShareShown: true, dndShown: false, trayNoticeShown: false },
    log: { level: 'info' },
    ...over,
  };
}

/** A shallow-per-group merge of a patch, as Go's Settings.Update applies it. */
function applyPatch(v: SettingsView, p: SettingsPatch): SettingsView {
  const next: SettingsView = { ...v };
  for (const [k, value] of Object.entries(p) as [string, unknown][]) {
    const cur: unknown = (v as unknown as Record<string, unknown>)[k];
    (next as unknown as Record<string, unknown>)[k] =
      typeof value === 'object' && value !== null && typeof cur === 'object' && cur !== null
        ? { ...cur, ...value }
        : value;
  }
  return next;
}

const SERVERS: ServerInfo[] = [
  {
    origin: testOrigin(),
    name: 'Test server',
    addedAt: '2026-10-01T00:00:00Z',
    user: { id: 'u1', username: 'alex', role: 'user' },
    device: { id: 'd1', name: 'Test PC' },
    id: 'k3m9p2qx',
    active: true,
    linked: true,
  },
  {
    origin: 'https://other.example.org',
    name: 'Other',
    addedAt: '2026-10-02T00:00:00Z',
    user: null,
    device: null,
    id: 'a1b2c3d4',
    active: false,
    linked: false,
  },
];

const CAPS: ShareCaps = {
  canShare: true,
  encoders: ['nvenc', 'mf-sw'],
  borderless: true,
  osBuild: 26100,
  audioBackend: 'process-loopback',
};

interface Setup {
  view?: SettingsView;
  status?: UpdateStatus;
  servers?: ServerInfo[];
  caps?: ShareCaps;
  hotkeys?: HotkeyStatus[];
  sharing?: boolean;
  path?: string;
  openH264?: OpenH264Control | null;
}

function renderSettings(o: Setup = {}) {
  const d: DesktopTestPlatform = createDesktopTestPlatform();
  let view = o.view ?? settingsView();
  d.fake.answers['settings.get'] = () => Promise.resolve(view);
  d.fake.answers['settings.update'] = (p: SettingsPatch) => {
    view = applyPatch(view, p);
    return Promise.resolve(view);
  };
  d.fake.answers['settings.hotkeys'] = o.hotkeys ?? [
    { id: 'pause', combo: view.hotkeys.pause, state: 'idle' },
    { id: 'stop', combo: view.hotkeys.stop, state: 'idle' },
  ];
  d.fake.answers['updates.status'] = o.status ?? updateStatus('idle');
  d.fake.answers['auth.servers'] = o.servers ?? SERVERS;
  d.fake.answers['share.capabilities'] = o.caps ?? CAPS;
  d.fake.answers['share.current'] = () => Promise.resolve(o.sharing === true ? shareView({ state: 'live' }) : null);
  d.fake.answers['audio.friendsHear'] = friendsHear({ live: false });
  d.fake.answers['audio.appRules'] = [];
  d.fake.answers['app.info'] = {
    version: '0.2.0',
    os: 'windows',
    osBuild: 26100,
    arch: 'amd64',
    publisherConnectionId: '',
    wizardDone: true,
    dev: false,
  };
  const r = renderDesktopPage(
    o.path ?? '/desktop/settings',
    d,
    <Toasts />,
    o.openH264 !== undefined ? { openH264: o.openH264 } : undefined,
  );
  return { ...r, d, view: () => view };
}

const section = (name: string): Promise<HTMLElement> => screen.findByRole('region', { name });

/** jsdom has no scrollIntoView: each test gets a recording one. */
let scroll = vi.fn();

beforeEach(() => {
  scroll = vi.fn();
  Element.prototype.scrollIntoView = scroll;
});

afterEach(() => {
  Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
});

describe('the page', () => {
  it('has the six sections with their anchors and links to them', async () => {
    renderSettings();
    await section('Sharing');
    for (const id of ['servers', 'sharing', 'audio', 'app', 'updates', 'about']) {
      expect(document.getElementById(id), id).not.toBeNull();
    }
    const nav = screen.getByRole('navigation', { name: 'Settings sections' });
    expect(
      within(nav)
        .getAllByRole('link')
        .map((a) => a.getAttribute('href')),
    ).toEqual(['#servers', '#sharing', '#audio', '#app', '#updates', '#about']);
  });

  it('shows the section of the address (#servers from the account menu and the Offline screen)', async () => {
    renderSettings({ path: '/desktop/settings#servers' });
    await screen.findByRole('region', { name: 'Your servers' });
    await waitFor(() => {
      expect(scroll).toHaveBeenCalled();
    });
    expect(scroll.mock.contexts[0]).toBe(document.getElementById('servers'));
  });
});

describe('Settings → Servers', () => {
  it('lists the servers (ServerSwitcher) with Switch, Sign out, Remove and Add', async () => {
    renderSettings();
    const list = await section('Your servers');
    expect(list.closest('#servers')).not.toBeNull();
    expect(await within(list).findByText(/Linked as alex · Active/)).toBeInTheDocument();
    expect(within(list).getByText(/Not linked/)).toBeInTheDocument();
    expect(within(list).getByRole('button', { name: 'Switch' })).toBeInTheDocument();
    expect(within(list).getByRole('button', { name: 'Sign out of Test server' })).toBeInTheDocument();
    expect(within(list).getAllByRole('button', { name: 'Remove' })).toHaveLength(2);
    expect(within(list).getByRole('button', { name: 'Add a server' })).toBeInTheDocument();
  });

  it('Link again for the active server when it is not linked: Auth.Link, then the link step', async () => {
    const user = userEvent.setup();
    const unlinked = SERVERS.map((s) => (s.active ? { ...s, linked: false, user: null, device: null } : s));
    const { d, router } = renderSettings({ servers: unlinked });
    const list = await section('Your servers');
    await user.click(await within(list).findByRole('button', { name: 'Link again' }));
    expect(d.fake.callsOf('auth.link')).toEqual([[]]);
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/desktop/setup');
    });
  });
});

describe('Settings → Sharing', () => {
  it('sets the default "Optimize for" of this server and the Sharing pill', async () => {
    const user = userEvent.setup();
    const origin = testOrigin();
    const { d } = renderSettings({ view: settingsView({ serverPrefs: { [origin]: { preset: 'game' } } }) });
    const sharing = await section('Sharing');
    const preset = within(sharing).getByRole('combobox', { name: 'Optimize for, on this server' });
    await waitFor(() => {
      expect(preset).toHaveValue('game');
    });
    await user.selectOptions(preset, 'Text');
    const pill = within(sharing).getByRole('group', { name: 'Sharing pill' });
    expect(within(pill).getByRole('radio', { name: 'Only when Windows shows no border' })).toBeChecked();
    await user.click(within(pill).getByRole('radio', { name: 'Always' }));
    expect(d.fake.callsOf('settings.update')).toEqual([
      [{ serverPrefs: { [origin]: { preset: 'text' } } }],
      [{ pill: { mode: 'always' } }],
    ]);
    await waitFor(() => {
      expect(within(pill).getByRole('radio', { name: 'Always' })).toBeChecked();
    });
  });

  it('shows the hotkeys with their status', async () => {
    renderSettings({
      hotkeys: [
        { id: 'pause', combo: 'Ctrl+Alt+Shift+F9', state: 'idle' },
        { id: 'stop', combo: 'Ctrl+Alt+Shift+F10', state: 'taken' },
      ],
    });
    const sharing = await section('Sharing');
    expect(await within(sharing).findByText('Ctrl+Alt+Shift+F9')).toBeInTheDocument();
    expect(within(sharing).getByText('Ctrl+Alt+Shift+F10')).toBeInTheDocument();
    expect(await within(sharing).findByText('Works while you share')).toBeInTheDocument();
    expect(within(sharing).getByText('Unavailable: another app uses Ctrl+Alt+Shift+F10')).toBeInTheDocument();
  });

  it('HotkeyField records a combination, tests it and saves it; it warns when it types a character', async () => {
    const user = userEvent.setup();
    const { d } = renderSettings();
    d.fake.answers['settings.testHotkey'] = (combo: string) =>
      Promise.resolve({ id: '', combo, state: 'idle', typesChar: true });
    const sharing = await section('Sharing');
    await user.click(
      await within(sharing).findByRole('button', { name: 'Change the hotkey: Pause and resume sharing' }),
    );
    const recorder = within(sharing).getByRole('textbox', { name: /New combination for Pause and resume sharing/ });
    await waitFor(() => {
      expect(recorder).toHaveFocus();
    });
    // One modifier with a letter isn't enough; the Windows key is never allowed.
    fireEvent.keyDown(recorder, { code: 'KeyP', key: 'p', ctrlKey: true });
    expect(within(sharing).getByRole('alert')).toHaveTextContent(/Use two of Ctrl, Alt and Shift/);
    fireEvent.keyDown(recorder, { code: 'KeyP', key: 'p', ctrlKey: true, metaKey: true });
    expect(within(sharing).getByRole('alert')).toHaveTextContent("The Windows key can't be part of an isshoni hotkey.");
    expect(d.fake.callsOf('settings.testHotkey')).toEqual([]);
    fireEvent.keyDown(recorder, { code: 'KeyP', key: 'p', ctrlKey: true, altKey: true });
    await waitFor(() => {
      expect(d.fake.callsOf('settings.update')).toEqual([[{ hotkeys: { pause: 'Ctrl+Alt+P' } }]]);
    });
    expect(d.fake.callsOf('settings.testHotkey')).toEqual([['Ctrl+Alt+P']]);
    expect(
      await within(sharing).findByText(
        'This combination types a character on your keyboard layout; typing it during a share would trigger the hotkey.',
      ),
    ).toBeInTheDocument();
    expect(await within(sharing).findByText('Ctrl+Alt+P')).toBeInTheDocument();
    // The statuses are read again after the save.
    expect(d.fake.callsOf('settings.hotkeys').length).toBeGreaterThanOrEqual(2);
  });

  it('HotkeyField does not save a taken combination or the other hotkey, and Escape cancels', async () => {
    const user = userEvent.setup();
    const { d } = renderSettings();
    d.fake.answers['settings.testHotkey'] = (combo: string) => Promise.resolve({ id: '', combo, state: 'taken' });
    const sharing = await section('Sharing');
    await user.click(await within(sharing).findByRole('button', { name: 'Change the hotkey: Stop sharing' }));
    const recorder = within(sharing).getByRole('textbox', { name: /New combination for Stop sharing/ });
    fireEvent.keyDown(recorder, { code: 'F9', key: 'F9', ctrlKey: true, altKey: true, shiftKey: true });
    expect(within(sharing).getByRole('alert')).toHaveTextContent('Pause and Stop need different combinations.');
    fireEvent.keyDown(recorder, { code: 'F11', key: 'F11', ctrlKey: true });
    expect(await within(sharing).findByText('Unavailable: another app uses Ctrl+F11')).toBeInTheDocument();
    expect(d.fake.callsOf('settings.update')).toEqual([]);
    fireEvent.keyDown(recorder, { code: 'Escape', key: 'Escape' });
    expect(await within(sharing).findByRole('button', { name: 'Change the hotkey: Stop sharing' })).toBeInTheDocument();
    expect(d.fake.callsOf('settings.testHotkey')).toEqual([['Ctrl+F11']]);
  });

  it('Show the first-share sheet again clears tips.firstShareShown', async () => {
    const user = userEvent.setup();
    const { d } = renderSettings();
    const sharing = await section('Sharing');
    const again = within(sharing).getByRole('button', { name: 'Show the first-share sheet again' });
    await waitFor(() => {
      expect(again).toBeEnabled();
    });
    await user.click(again);
    expect(d.fake.callsOf('settings.update')).toEqual([[{ tips: { firstShareShown: false } }]]);
    expect(await within(sharing).findByText('The first-share sheet shows before your next share.')).toBeVisible();
    expect(again).toBeDisabled();
  });

  it('lists the encoders 04 found, or why none works', async () => {
    const { unmount } = renderSettings();
    const encoders = await screen.findByTestId('encoders');
    expect(
      await within(encoders).findByText('isshoni can use: NVIDIA NVENC, Media Foundation (software)'),
    ).toBeInTheDocument();
    unmount();
    renderSettings({ caps: { ...CAPS, canShare: false, encoders: [], reason: 'no_encoder' } });
    expect(
      await within(await screen.findByTestId('encoders')).findByText('No H.264 encoder works on this PC.'),
    ).toBeVisible();
  });

  it('has no OpenH264 opt-in while Go offers none', async () => {
    renderSettings();
    await screen.findByTestId('encoders');
    expect(screen.queryByTestId('openh264')).not.toBeInTheDocument();
    expect(screen.queryByText(/OpenH264 Video Codec provided by Cisco/)).not.toBeInTheDocument();
  });

  it('the OpenH264 opt-in: Cisco notice, enable (a failed download says so), attribution in About, turn off', async () => {
    const user = userEvent.setup();
    let status: OpenH264Status = { state: 'off', version: '2.7.0' };
    const control = {
      status: vi.fn(() => Promise.resolve(status)),
      enable: vi
        .fn<OpenH264Control['enable']>()
        .mockRejectedValueOnce(new BridgeError('engine.dependency', { reason: 'download' }))
        .mockImplementation(() => {
          status = { state: 'on', version: '2.7.0' };
          return Promise.resolve(status);
        }),
      disable: vi.fn(() => {
        status = { state: 'off', version: '2.7.0' };
        return Promise.resolve(status);
      }),
    } satisfies OpenH264Control;
    renderSettings({ openH264: control });
    const optIn = await screen.findByTestId('openh264');
    expect(within(optIn).getByText(/isshoni can use Cisco's OpenH264 2.7.0/)).toBeInTheDocument();
    expect(within(optIn).getByText('OpenH264 Video Codec provided by Cisco Systems, Inc.')).toBeInTheDocument();
    expect(within(optIn).getByRole('link', { name: "Cisco's license for OpenH264" })).toHaveAttribute(
      'href',
      OPENH264_LICENSE_URL,
    );
    await user.click(within(optIn).getByRole('button', { name: 'Enable Cisco OpenH264…' }));
    expect(await within(optIn).findByRole('alert')).toHaveTextContent(
      "Couldn't download OpenH264 from Cisco. Check your connection and try again.",
    );
    await user.click(within(optIn).getByRole('button', { name: 'Enable Cisco OpenH264…' }));
    expect(
      await within(optIn).findByText('Cisco OpenH264 2.7.0 is installed. isshoni uses it when no other encoder works.'),
    ).toBeInTheDocument();
    const about = await section('About');
    expect(within(about).getByText('OpenH264 Video Codec provided by Cisco Systems, Inc.')).toBeInTheDocument();
    await user.click(within(optIn).getByRole('button', { name: 'Turn off and delete' }));
    expect(control.disable).toHaveBeenCalledOnce();
    expect(await within(optIn).findByRole('button', { name: 'Enable Cisco OpenH264…' })).toBeInTheDocument();
    expect(within(about).queryByText('OpenH264 Video Codec provided by Cisco Systems, Inc.')).not.toBeInTheDocument();
  });

  it.each([
    ['engine.dependency', { reason: 'download' }, "Couldn't download OpenH264 from Cisco"],
    ['engine.dependency', { reason: 'corrupt' }, "didn't match the one isshoni expects"],
    ['engine.dependency', { reason: 'missing' }, 'The OpenH264 file is gone'],
    ['engine.dependency', { reason: 'version' }, 'the wrong version'],
    ['engine.dependency', { reason: 'blocked_by_policy' }, 'blocked OpenH264'],
    ['engine.cancelled', {}, 'The download was stopped.'],
    ['engine.timeout', {}, 'The download took too long. Try again.'],
    ['engine.internal', {}, "Something went wrong in isshoni's video engine."],
  ])('the OpenH264 failure %s %o has its text', (code, params, text) => {
    expect(openH264ErrorText(i18n.t, new BridgeError(code, params))).toContain(text);
  });
});

describe('Settings → App', () => {
  it('the switches save their settings; the tray button opens Windows settings', async () => {
    const user = userEvent.setup();
    const { d } = renderSettings();
    const app = await section('App');
    const autostart = within(app).getByRole('switch', { name: 'Start isshoni when I sign in to Windows' });
    await waitFor(() => {
      expect(autostart).toBeEnabled();
    });
    expect(autostart).not.toBeChecked();
    expect(within(app).getByRole('switch', { name: /Hide isshoni windows from screen capture/ })).toBeChecked();
    expect(within(app).getByRole('switch', { name: 'Show notifications' })).toBeChecked();
    await user.click(autostart);
    await user.click(within(app).getByRole('switch', { name: /Keep isshoni running in the tray/ }));
    await user.click(within(app).getByRole('switch', { name: /Hide isshoni windows from screen capture/ }));
    await user.click(within(app).getByRole('switch', { name: 'Show notifications' }));
    expect(d.fake.callsOf('settings.update')).toEqual([
      [{ autostart: true }],
      [{ closeToTray: true }],
      [{ hideFromCapture: false }],
      [{ notifications: false }],
    ]);
    await waitFor(() => {
      expect(autostart).toBeChecked();
    });
    await user.click(within(app).getByRole('button', { name: 'Keep the tray icon visible' }));
    expect(d.fake.callsOf('app.openOSSettings')).toEqual([['taskbar']]);
  });

  it('a save that fails says why and shows the saved value again', async () => {
    const user = userEvent.setup();
    const { d } = renderSettings();
    d.fake.answers['settings.update'] = () => Promise.reject(new BridgeError('not_implemented'));
    const app = await section('App');
    const autostart = within(app).getByRole('switch', { name: 'Start isshoni when I sign in to Windows' });
    await waitFor(() => {
      expect(autostart).toBeEnabled();
    });
    await user.click(autostart);
    expect(await screen.findByText("This part of isshoni isn't finished in this build.")).toBeInTheDocument();
    await waitFor(() => {
      expect(autostart).not.toBeChecked();
    });
  });

  it('Quit isshoni quits at once without a share, and asks first while one is live', async () => {
    const user = userEvent.setup();
    const first = renderSettings();
    await user.click(within(await section('App')).getByRole('button', { name: 'Quit isshoni' }));
    expect(first.d.fake.callsOf('app.quit')).toEqual([[false]]);
    first.unmount();

    const { d } = renderSettings({ sharing: true });
    const app = await section('App');
    await waitFor(() => {
      expect(d.fake.callsOf('share.current').length).toBeGreaterThan(0);
    });
    await act(() => Promise.resolve());
    await user.click(within(app).getByRole('button', { name: 'Quit isshoni' }));
    const dialog = await screen.findByRole('dialog', { name: 'Stop sharing and quit?' });
    expect(d.fake.callsOf('app.quit')).toEqual([]);
    await user.click(within(dialog).getByRole('button', { name: 'Stop sharing and quit' }));
    expect(d.fake.callsOf('app.quit')).toEqual([[true]]);
  });
});

describe('Settings → Updates (03 §16.2)', () => {
  const statusLine = async (): Promise<HTMLElement> => within(await section('Updates')).findByTestId('update-status');

  it('up_to_date: "Up to date, checked <time>"', async () => {
    const checkedAt = '2026-11-05T10:00:00Z';
    renderSettings({ status: updateStatus('up_to_date', { checkedAt }) });
    const time = new Intl.DateTimeFormat('en', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(checkedAt));
    expect(await statusLine()).toHaveTextContent(`Up to date, checked ${time}`);
  });

  it('downloading: the version and the progress', async () => {
    renderSettings({ status: updateStatus('downloading', { available: '0.3.0', progress: 0.42 }) });
    expect(await statusLine()).toHaveTextContent('Downloading 0.3.0… 42 %');
  });

  it('ready: Restart and update runs Updates.Install()', async () => {
    const user = userEvent.setup();
    const { d } = renderSettings({ status: updateStatus('ready', { available: '0.3.0' }) });
    const line = await statusLine();
    expect(line).toHaveTextContent('isshoni 0.3.0 is ready');
    await user.click(within(line).getByRole('button', { name: 'Restart and update' }));
    expect(d.fake.callsOf('updates.install')).toEqual([[]]);
  });

  it('ready while a share is live: "Finish sharing first", and Go answering busy says the same', async () => {
    const live = renderSettings({ status: updateStatus('ready', { available: '0.3.0' }), sharing: true });
    const line = await statusLine();
    expect(await within(line).findByRole('button', { name: 'Finish sharing first' })).toBeDisabled();
    expect(live.d.fake.callsOf('updates.install')).toEqual([]);
    live.unmount();

    const user = userEvent.setup();
    const { d } = renderSettings({ status: updateStatus('ready', { available: '0.3.0' }) });
    d.fake.answers['updates.install'] = () => Promise.reject(new BridgeError('busy'));
    await user.click(within(await statusLine()).getByRole('button', { name: 'Restart and update' }));
    expect(await within(await statusLine()).findByRole('button', { name: 'Finish sharing first' })).toBeDisabled();
  });

  it('error manifest_expired: the out-of-date text, the clock line and the help link', async () => {
    renderSettings({ status: updateStatus('error', { errorCode: 'manifest_expired' }) });
    const line = await statusLine();
    expect(within(line).getByRole('alert')).toHaveTextContent(
      "Update info is out of date; get the new version from your server's Download page",
    );
    expect(within(line).getByText("Check that your PC's clock is right")).toBeInTheDocument();
    expect(within(line).getByRole('link', { name: 'How isshoni updates' })).toHaveAttribute('href', UPDATES_HELP);
    expect(UPDATES_HELP).toBe('https://moonwx.github.io/isshoni/download#windows-updates');
  });

  it('error install_failed: "The update didn\'t install" with the help link', async () => {
    renderSettings({ status: updateStatus('error', { errorCode: 'install_failed' }) });
    const line = await statusLine();
    expect(within(line).getByRole('alert')).toHaveTextContent("The update didn't install");
    expect(within(line).getByRole('link', { name: 'How isshoni updates' })).toHaveAttribute('href', UPDATES_HELP);
  });

  it('another error: desktop.update.error.<code>, with Check now', async () => {
    renderSettings({ status: updateStatus('error', { errorCode: 'network' }) });
    const updates = await section('Updates');
    expect(await within(updates).findByRole('alert')).toHaveTextContent(
      "isshoni couldn't reach GitHub to check for updates. Check your connection.",
    );
    expect(within(updates).getByRole('button', { name: 'Check now' })).toBeInTheDocument();
  });

  it('off with policyOff: the setting greyed out with "Turned off by your administrator"', async () => {
    renderSettings({
      status: updateStatus('off', { policyOff: true }),
      view: settingsView({ updates: { check: true, channel: 'stable', policyOff: true } }),
    });
    const updates = await section('Updates');
    const check = within(updates).getByRole('switch', { name: 'Check for updates automatically' });
    await waitFor(() => {
      expect(check).toHaveAccessibleDescription('Turned off by your administrator');
    });
    expect(check).toBeDisabled();
    expect(check).not.toBeChecked();
    expect(await within(updates).findByTestId('update-status')).toHaveTextContent('Turned off by your administrator');
  });

  it('off without policyOff: "Turned off", and the switch works', async () => {
    const user = userEvent.setup();
    const { d } = renderSettings({
      status: updateStatus('off'),
      view: settingsView({ updates: { check: false, channel: 'stable', policyOff: false } }),
    });
    expect(await statusLine()).toHaveTextContent('Turned off');
    const check = within(await section('Updates')).getByRole('switch', { name: 'Check for updates automatically' });
    await waitFor(() => {
      expect(check).toBeEnabled();
    });
    await user.click(check);
    expect(d.fake.callsOf('settings.update')).toEqual([[{ updates: { check: true } }]]);
  });

  it('disabled: "Updates are off in this build"', async () => {
    renderSettings({ status: updateStatus('disabled', { errorCode: 'not_release' }) });
    expect(await statusLine()).toHaveTextContent('Updates are off in this build');
  });

  it('idle and checking', async () => {
    const { d } = renderSettings({ status: updateStatus('idle') });
    expect(await statusLine()).toHaveTextContent('Not checked yet');
    act(() => {
      d.fake.emit('update.state', updateStatus('checking'));
    });
    expect(await statusLine()).toHaveTextContent('Checking for updates…');
  });

  it('the beta channel switch sets updates.channel', async () => {
    const user = userEvent.setup();
    const { d } = renderSettings();
    const beta = within(await section('Updates')).getByRole('switch', {
      name: 'Get release candidates (beta channel)',
    });
    await waitFor(() => {
      expect(beta).toBeEnabled();
    });
    expect(beta).not.toBeChecked();
    await user.click(beta);
    await waitFor(() => {
      expect(beta).toBeChecked();
    });
    await user.click(beta);
    expect(d.fake.callsOf('settings.update')).toEqual([
      [{ updates: { channel: 'beta' } }],
      [{ updates: { channel: 'stable' } }],
    ]);
  });

  it('Check now asks Go and shows its answer, or its error code', async () => {
    const user = userEvent.setup();
    const { d } = renderSettings({ status: updateStatus('idle') });
    d.fake.answers['updates.check'] = updateStatus('up_to_date');
    const updates = await section('Updates');
    await user.click(within(updates).getByRole('button', { name: 'Check now' }));
    expect(await statusLine()).toHaveTextContent(/^Up to date$/);
    d.fake.answers['updates.check'] = () => Promise.reject(new BridgeError('update.rate_limited'));
    await user.click(within(updates).getByRole('button', { name: 'Check now' }));
    expect(await within(updates).findByText('GitHub asked isshoni to wait. It checks again later.')).toBeVisible();
    expect(d.fake.callsOf('updates.check')).toHaveLength(2);
    expect(within(updates).getByText('Updates come only from GitHub Releases, never from your server.')).toBeVisible();
  });

  it('every update.* bridge code has its desktop.update.error.<code> text (03 §6.4, README §3 item 42)', () => {
    const prefix = codes.UpdatePrefix;
    const values: unknown[] = Object.values(codes);
    const updateCodes = values.filter(
      (v): v is string => typeof v === 'string' && v.startsWith(prefix) && v !== prefix,
    );
    expect(updateCodes.length).toBeGreaterThanOrEqual(17);
    for (const code of updateCodes) {
      expect(i18n.exists(`desktop.update.error.${code.slice(prefix.length)}`), code).toBe(true);
    }
  });
});

describe('the required update banner (03 §16.2)', () => {
  it('shows "An important update is ready" with Restart and update, and can not be dismissed', async () => {
    const user = userEvent.setup();
    const d = createDesktopTestPlatform();
    d.fake.answers['updates.status'] = updateStatus('ready', { available: '0.3.0', required: true });
    d.fake.answers['share.current'] = null;
    renderDesktopPage('/', d, <RequiredUpdateBanner />);
    const banner = await screen.findByRole('region', { name: 'Important update' });
    expect(banner).toHaveTextContent('An important update is ready');
    expect(within(banner).getAllByRole('button')).toHaveLength(1);
    await user.click(within(banner).getByRole('button', { name: 'Restart and update' }));
    expect(d.fake.callsOf('updates.install')).toEqual([[]]);
  });

  it('only while ready and required', async () => {
    const d = createDesktopTestPlatform();
    d.fake.answers['updates.status'] = updateStatus('ready', { available: '0.3.0' });
    d.fake.answers['share.current'] = null;
    renderDesktopPage('/', d, <RequiredUpdateBanner />);
    await screen.findByRole('heading', { name: 'home' });
    await waitFor(() => {
      expect(d.fake.callsOf('updates.status')).toHaveLength(1);
    });
    expect(screen.queryByRole('region', { name: 'Important update' })).not.toBeInTheDocument();
    act(() => {
      d.fake.emit('update.state', updateStatus('ready', { available: '0.3.0', required: true }));
    });
    expect(await screen.findByRole('region', { name: 'Important update' })).toBeInTheDocument();
    act(() => {
      d.fake.emit('update.state', updateStatus('downloading', { available: '0.3.1', required: true }));
    });
    expect(screen.queryByRole('region', { name: 'Important update' })).not.toBeInTheDocument();
  });
});

describe('Settings → About', () => {
  it('shows the versions and the engine; Copy version info copies them and nothing else', async () => {
    const user = userEvent.setup();
    renderSettings();
    const about = await section('About');
    await waitFor(() => {
      expect(within(about).getByText('NVIDIA NVENC, Media Foundation (software)')).toBeInTheDocument();
    });
    expect(within(about).getByText('0.2.0')).toBeInTheDocument();
    expect(within(about).getByText('26100')).toBeInTheDocument();
    expect(within(about).getByText('Per app (voice apps kept out)')).toBeInTheDocument();
    await user.click(within(about).getByRole('button', { name: 'Copy version info' }));
    const copied = await navigator.clipboard.readText();
    expect(copied).toBe(
      [
        'isshoni 0.2.0 (amd64)',
        'Windows build 26100',
        `WebView2 ${webViewVersion(navigator.userAgent) ?? 'unknown'}`,
        'Engine: encoders nvenc, mf-sw; sound process-loopback',
      ].join('\n'),
    );
    expect(copied).not.toContain(testOrigin());
    expect(await screen.findByText('Version info copied')).toBeInTheDocument();
  });

  it('opens the logs folder and the notices, sets the log detail, and links to privacy and code signing', async () => {
    const user = userEvent.setup();
    const { d } = renderSettings();
    const about = await section('About');
    await user.click(within(about).getByRole('button', { name: 'Open logs folder' }));
    await user.click(within(about).getByRole('button', { name: 'Third-party notices' }));
    expect(d.fake.callsOf('app.openLogsFolder')).toEqual([[]]);
    expect(d.fake.callsOf('app.openNotices')).toEqual([[]]);
    const level = within(about).getByRole('combobox', { name: 'Log detail' });
    await waitFor(() => {
      expect(level).toBeEnabled();
    });
    await user.selectOptions(level, 'Detailed (for a bug report)');
    expect(d.fake.callsOf('settings.update')).toEqual([[{ log: { level: 'debug' } }]]);
    expect(within(about).getByRole('link', { name: 'Privacy' })).toHaveAttribute('href', PRIVACY_DESKTOP_URL);
    expect(within(about).getByRole('link', { name: 'Code signing' })).toHaveAttribute('href', CODE_SIGNING_URL);
    expect(PRIVACY_DESKTOP_URL).toBe('https://moonwx.github.io/isshoni/privacy#desktop');
    await user.click(within(about).getByRole('link', { name: 'Privacy' }));
    expect(d.opened).toEqual([PRIVACY_DESKTOP_URL]);
  });

  it('shows the bundled web app licenses in a dialog', async () => {
    const user = userEvent.setup();
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response('react MIT\n', { status: 200 }));
    try {
      renderSettings();
      const about = await section('About');
      await user.click(within(about).getByRole('button', { name: 'Web app licenses' }));
      const dialog = await screen.findByRole('dialog', { name: 'Web app licenses' });
      expect(await within(dialog).findByRole('textbox', { name: 'Web app licenses' })).toHaveValue('react MIT\n');
      expect(fetchMock).toHaveBeenCalledWith('/licenses.txt');
    } finally {
      fetchMock.mockRestore();
    }
  });

  it('versionInfoText and webViewVersion', () => {
    const ua =
      'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.2903.70';
    expect(webViewVersion(ua)).toBe('131.0.2903.70');
    expect(webViewVersion('Mozilla/5.0 Chrome/131.0.0.0')).toBeNull();
    expect(
      versionInfoText({ version: '0.2.0', arch: '', osBuild: 0, webView: null, encoders: [], audioBackend: null }),
    ).toBe('isshoni 0.2.0\nWindows build unknown\nWebView2 unknown\nEngine: encoders none; sound unknown');
  });
});
