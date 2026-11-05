// Share safety in the desktop's share UI (docs/m2/03 §10.2, §10.3, §11.2, §11.3, §11.6, §22.3; README WA80): Pause in
// the panel's bar with the hotkeys in the buttons' tooltips, "Pause hotkey unavailable: another app uses …" with the
// way to Settings, Pause disabled once the server answered feature_disabled, the first-share sheet's hotkey line, and
// the close-to-tray notice at the top of the window, reported shown only while the page is visible, and the
// lock-prompt bar of a late unlock (ShareView.lockPrompt, README WF17).
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { HotkeyStatus, ShareView } from '../../platform/desktop/bridge';
import { BridgeError } from '../../platform/desktop/errors';
import { shareView } from '../../platform/desktop/testing/shareView';
import { createTestServices, renderRoute } from '../../test/render';
import { NativeShareButton } from './NativeShareButton';
import { NativeSharePanel } from './NativeSharePanel';
import { nativeSetup, PICKED, type NativeSetup } from './testing';
import { loadHotkeys } from './useHotkeys';
import { WindowNotice } from './WindowNotice';

const REGISTERED: HotkeyStatus[] = [
  { id: 'pause', combo: 'Ctrl+Alt+Shift+F9', state: 'registered' },
  { id: 'stop', combo: 'Ctrl+Alt+Shift+F10', state: 'registered' },
];

afterEach(() => {
  vi.restoreAllMocks();
});

async function renderPanel(
  current: ShareView,
  hotkeys: HotkeyStatus[] | 'not_implemented' = REGISTERED,
  onStop: () => unknown = vi.fn(),
): Promise<NativeSetup> {
  const setup = await nativeSetup({ current });
  setup.fake.answers['settings.hotkeys'] =
    hotkeys === 'not_implemented' ? () => Promise.reject(new BridgeError('not_implemented')) : hotkeys;
  setup.fake.answers['settings.get'] = { hotkeys: { pause: 'Ctrl+Alt+Shift+F9', stop: 'Ctrl+Alt+Shift+F10' } };
  const services = createTestServices({ platform: setup.platform });
  renderRoute({
    services,
    routes: [
      {
        path: '/',
        element: <NativeSharePanel share={setup.sharing.current()} onStop={onStop} watchers={[]} store={setup.store} />,
      },
      { path: '/desktop/settings', element: <h1>Settings</h1> },
    ],
  });
  return setup;
}

const region = () => screen.getByRole('region', { name: 'Your share' });

describe('Pause in the panel (docs/m2/03 §10.3, §11.3)', () => {
  it('live: Pause pauses through Share.SetPaused, with the hotkeys in the tooltips', async () => {
    const user = userEvent.setup();
    const setup = await renderPanel(shareView());
    const pause = within(region()).getByRole('button', { name: 'Pause' });
    await waitFor(() => {
      expect(pause).toHaveAttribute('title', 'Pause (Ctrl+Alt+Shift+F9)');
    });
    expect(within(region()).getByRole('button', { name: 'Stop sharing' })).toHaveAttribute(
      'title',
      'Stop (Ctrl+Alt+Shift+F10)',
    );
    await user.click(pause);
    expect(setup.fake.callsOf('share.setPaused')).toEqual([[true]]);
  });

  it('paused: Resume instead of Pause, also named with its hotkey', async () => {
    await renderPanel(shareView({ state: 'paused' }));
    expect(within(region()).queryByRole('button', { name: 'Pause' })).not.toBeInTheDocument();
    const resume = within(region()).getByRole('button', { name: 'Resume' });
    await waitFor(() => {
      expect(resume).toHaveAttribute('title', 'Resume (Ctrl+Alt+Shift+F9)');
    });
  });

  it('no Pause while the share starts', async () => {
    await renderPanel(shareView({ state: 'starting', liveSince: null }));
    expect(within(region()).queryByRole('button', { name: 'Pause' })).not.toBeInTheDocument();
  });

  it('a server without share.pause: Pause is disabled with "Pause needs a newer server", no error toast', async () => {
    const user = userEvent.setup();
    const setup = await renderPanel(shareView());
    setup.fake.answers['share.setPaused'] = () => Promise.reject(new BridgeError('feature_disabled'));
    const pause = within(region()).getByRole('button', { name: 'Pause' });
    await user.click(pause);
    await waitFor(() => {
      expect(pause).toBeDisabled();
    });
    expect(pause).toHaveAttribute('title', 'Pause needs a newer server');
  });

  it('a combination another app has: said under the bar, left out of the tooltip, with the way to Settings', async () => {
    const user = userEvent.setup();
    await renderPanel(shareView(), [
      { id: 'pause', combo: 'Ctrl+Alt+Shift+F9', state: 'taken' },
      { id: 'stop', combo: 'Ctrl+Alt+Shift+F10', state: 'registered' },
    ]);
    expect(
      await within(region()).findByText('Pause hotkey unavailable: another app uses Ctrl+Alt+Shift+F9'),
    ).toBeInTheDocument();
    expect(within(region()).getByRole('button', { name: 'Pause' })).toHaveAttribute('title', 'Pause');
    await user.click(within(region()).getByRole('button', { name: 'Change it in Settings' }));
    expect(await screen.findByRole('heading', { name: 'Settings' })).toBeInTheDocument();
  });

  it('until Settings.Hotkeys is bound, the settings’ combinations name the buttons', async () => {
    await renderPanel(shareView(), 'not_implemented');
    await waitFor(() => {
      expect(within(region()).getByRole('button', { name: 'Pause' })).toHaveAttribute(
        'title',
        'Pause (Ctrl+Alt+Shift+F9)',
      );
    });
  });
});

describe('the lock-prompt bar (docs/m2/03 §11.6; README WF17)', () => {
  const PROMPT = 'Sharing is still paused since you locked your PC.';

  it('asks while ShareView.lockPrompt is set: Resume sharing resumes through Share.SetPaused', async () => {
    const user = userEvent.setup();
    const setup = await renderPanel(shareView({ state: 'paused', lockPrompt: true }));
    // The panel's status line, a live region whatever it says, so the prompt's text is announced when it comes.
    const status = within(region()).getByText(PROMPT);
    expect(status).toHaveAttribute('data-lock-prompt');
    expect(status).toHaveAttribute('role', 'status');
    const resume = within(region()).getByRole('button', { name: 'Resume sharing' });
    await waitFor(() => {
      expect(resume).toHaveAttribute('title', 'Resume sharing (Ctrl+Alt+Shift+F9)');
    });
    expect(within(region()).getByRole('button', { name: 'Stop sharing' })).toHaveTextContent('Stop');
    expect(within(region()).queryByRole('button', { name: 'Pause' })).not.toBeInTheDocument();
    await user.click(resume);
    expect(setup.fake.callsOf('share.setPaused')).toEqual([[false]]);
    // Go ends the lock pause and the share goes live: the bar asks no more.
    act(() => {
      setup.fake.emit('share.state', shareView({ state: 'live', lockPrompt: false }));
    });
    await waitFor(() => {
      expect(within(region()).queryByText(PROMPT)).not.toBeInTheDocument();
    });
    expect(within(region()).getByRole('button', { name: 'Pause' })).toBeInTheDocument();
  });

  it('its Stop is the panel’s Stop (the room session’s stopShare)', async () => {
    const user = userEvent.setup();
    const onStop = vi.fn();
    await renderPanel(shareView({ state: 'paused', lockPrompt: true }), REGISTERED, onStop);
    expect(within(region()).getByText(PROMPT)).toBeInTheDocument();
    await user.click(within(region()).getByRole('button', { name: 'Stop sharing' }));
    expect(onStop).toHaveBeenCalledTimes(1);
  });

  it('comes with the share.state that sets it, and a pause without it is the plain paused bar', async () => {
    const setup = await renderPanel(shareView({ state: 'paused' }));
    expect(within(region()).queryByText(PROMPT)).not.toBeInTheDocument();
    // The live region is there before the prompt: only its text changes.
    const status = within(region()).getByRole('status');
    expect(status).not.toHaveAttribute('data-lock-prompt');
    expect(within(region()).getByRole('button', { name: 'Resume' })).toBeInTheDocument();
    act(() => {
      setup.fake.emit('share.state', shareView({ state: 'paused', lockPrompt: true }));
    });
    expect(await within(region()).findByText(PROMPT)).toBe(status);
    expect(status).toHaveAttribute('data-lock-prompt');
    expect(within(region()).getByRole('button', { name: 'Resume sharing' })).toBeInTheDocument();
  });

  it('never shows for a share that isn’t paused', async () => {
    await renderPanel(shareView({ state: 'live', lockPrompt: true }));
    expect(within(region()).queryByText(PROMPT)).not.toBeInTheDocument();
    expect(within(region()).queryByRole('button', { name: 'Resume sharing' })).not.toBeInTheDocument();
  });
});

describe('loadHotkeys', () => {
  it('reads Settings.Hotkeys, else the settings as idle, else nothing', async () => {
    const setup = await nativeSetup();
    setup.fake.answers['settings.hotkeys'] = REGISTERED;
    expect(await loadHotkeys(setup.fake.bridge)).toEqual({ pause: REGISTERED[0], stop: REGISTERED[1] });
    setup.fake.answers['settings.hotkeys'] = () => Promise.reject(new BridgeError('not_implemented'));
    setup.fake.answers['settings.get'] = { hotkeys: { pause: 'Ctrl+Alt+Shift+F9', stop: '' } };
    expect(await loadHotkeys(setup.fake.bridge)).toEqual({
      pause: { id: 'pause', combo: 'Ctrl+Alt+Shift+F9', state: 'idle' },
      stop: null,
    });
    setup.fake.answers['settings.get'] = () => Promise.reject(new BridgeError('internal'));
    expect(await loadHotkeys(setup.fake.bridge)).toEqual({ pause: null, stop: null });
  });
});

describe('the first-share sheet’s hotkey line (docs/m2/03 §10.2)', () => {
  async function openSheet(hotkeys: HotkeyStatus[]): Promise<void> {
    const user = userEvent.setup();
    const setup = await nativeSetup({ picked: { ...PICKED, confirm: ['first-share'] } });
    setup.fake.answers['settings.hotkeys'] = hotkeys;
    const services = createTestServices({ platform: setup.platform });
    renderRoute({
      services,
      routes: [{ path: '/', element: <NativeShareButton onStart={() => Promise.resolve()} store={setup.store} /> }],
    });
    await user.click(screen.getByRole('button', { name: 'Share' }));
    await user.click(
      within(screen.getByRole('dialog', { name: 'Share your screen' })).getByRole('button', {
        name: 'Share',
      }),
    );
  }

  const first = () => screen.getByRole('dialog', { name: 'Ready to go live?' });

  it('names both combinations, also from the tray icon', async () => {
    await openSheet(REGISTERED.map((h) => ({ ...h, state: 'idle' as const })));
    expect(
      await within(first()).findByText('Pause: Ctrl+Alt+Shift+F9 · Stop: Ctrl+Alt+Shift+F10, also from the tray icon'),
    ).toBeInTheDocument();
  });

  it('leaves out a hotkey that won’t work', async () => {
    await openSheet([
      { id: 'pause', combo: 'Ctrl+Alt+Shift+F9', state: 'taken' },
      { id: 'stop', combo: 'Ctrl+Alt+Shift+F10', state: 'idle' },
    ]);
    expect(await within(first()).findByText('Stop: Ctrl+Alt+Shift+F10, also from the tray icon')).toBeInTheDocument();
    expect(within(first()).queryByText(/Pause: /)).not.toBeInTheDocument();
  });

  it('says nothing when neither works', async () => {
    await openSheet([
      { id: 'pause', combo: 'Win+Q', state: 'invalid' },
      { id: 'stop', combo: 'Ctrl+Alt+Shift+F10', state: 'taken' },
    ]);
    await within(first()).findByText('Friends will see: Minecraft');
    expect(within(first()).queryByText(/also from the tray icon/)).not.toBeInTheDocument();
  });
});

describe('the window notice (docs/m2/03 §11.6)', () => {
  const NOTICE = { code: 'still-sharing', text: 'isshoni is still sharing. Stop it from the tray icon (^).' };

  async function renderNotice(pulled: unknown): Promise<NativeSetup> {
    const setup = await nativeSetup();
    setup.fake.answers['app.windowNotice'] = pulled;
    const services = createTestServices({ platform: setup.platform });
    renderRoute({ services, routes: [{ path: '/', element: <WindowNotice bridge={setup.fake.bridge} /> }] });
    return setup;
  }

  it('shows the pulled notice, reports it shown once, and goes away when dismissed', async () => {
    const user = userEvent.setup();
    const setup = await renderNotice(NOTICE);
    expect(await screen.findByText(NOTICE.text)).toBeInTheDocument();
    await waitFor(() => {
      expect(setup.fake.callsOf('app.windowNoticeShown')).toEqual([['still-sharing']]);
    });
    await user.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(screen.queryByText(NOTICE.text)).not.toBeInTheDocument();
    expect(setup.fake.callsOf('app.windowNoticeShown')).toHaveLength(1);
  });

  it('shows the notice of the window.notice event when the window opens', async () => {
    const setup = await renderNotice(null);
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
    act(() => {
      setup.fake.emit('window.notice' as never, NOTICE as never);
    });
    expect(await screen.findByText(NOTICE.text)).toBeInTheDocument();
    await waitFor(() => {
      expect(setup.fake.callsOf('app.windowNoticeShown')).toEqual([['still-sharing']]);
    });
  });

  it('reports nothing while the page is hidden, and reports once it is visible', async () => {
    const state = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
    const setup = await renderNotice(NOTICE);
    expect(await screen.findByText(NOTICE.text)).toBeInTheDocument();
    expect(setup.fake.callsOf('app.windowNoticeShown')).toEqual([]);
    state.mockReturnValue('visible');
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
    expect(setup.fake.callsOf('app.windowNoticeShown')).toEqual([['still-sharing']]);
  });

  it('pulls again when the page becomes visible: a window shown without the event still shows the notice', async () => {
    const state = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
    let due: unknown = null; // Go answers null while the window is hidden
    const setup = await renderNotice(() => Promise.resolve(due));
    await waitFor(() => {
      expect(setup.fake.callsOf('app.windowNotice')).toHaveLength(1);
    });
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
    due = NOTICE;
    state.mockReturnValue('visible');
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
    expect(await screen.findByText(NOTICE.text)).toBeInTheDocument();
    expect(setup.fake.callsOf('app.windowNotice')).toHaveLength(2);
    await waitFor(() => {
      expect(setup.fake.callsOf('app.windowNoticeShown')).toEqual([['still-sharing']]);
    });
  });

  it('a pulled answer that isn’t a notice shows nothing', async () => {
    const setup = await renderNotice(null);
    await waitFor(() => {
      expect(setup.fake.callsOf('app.windowNotice')).toHaveLength(1);
    });
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
    expect(setup.fake.callsOf('app.windowNoticeShown')).toEqual([]);
  });
});
