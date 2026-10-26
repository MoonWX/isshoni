// The native share panel (docs/m2/03 §10.3, §22.3 "Native panel"): the bar for each state of the share's view, the
// minimized line, the unreachable bar's Test connection URL, the Details disclosure (Ctrl+Shift+D, watchServerStats
// on open and off on close, no names), "Switched to <encoder>" for 5 s, Share sound only for a share started with
// sound, no "No sound captured" line, and a failed share with what to do (also a pick that timed out, and a new
// pick from the panel that fails before any share).
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { ShareView } from '../../platform/desktop/bridge';
import { BridgeError } from '../../platform/desktop/errors';
import { shareView } from '../../platform/desktop/testing/shareView';
import { createTestServices, renderRoute } from '../../test/render';
import type { ShareWatcher } from '../watchers';
import { NativeSharePanel } from './NativeSharePanel';
import { nativeSetup, ORIGIN, type NativeSetup } from './testing';
import { ENCODER_NOTICE_MS } from './useShareView';

const WATCHERS: ShareWatcher[] = [
  { userId: 'u_bea', name: 'Bea' },
  { userId: 'u_cy', name: '' },
];

afterEach(() => {
  vi.useRealTimers();
});

async function renderPanel(
  current: ShareView | null = shareView(),
  opts: { watchers?: ShareWatcher[]; withStart?: boolean; adopted?: boolean } = {},
): Promise<
  NativeSetup & {
    onStop: ReturnType<typeof vi.fn>;
    onStart: ReturnType<typeof vi.fn>;
    rerender: () => void;
    toasts: () => string[];
  }
> {
  const setup = await nativeSetup({ current });
  const onStop = vi.fn();
  const services = createTestServices({ platform: setup.platform });
  const onStart = vi.fn(() => Promise.resolve());
  const element = () => (
    <NativeSharePanel
      share={opts.adopted === false ? null : setup.sharing.current()}
      onStop={onStop}
      watchers={opts.watchers ?? WATCHERS}
      store={setup.store}
      {...(opts.withStart === true ? { onStart } : {})}
    />
  );
  const { router } = renderRoute({
    services,
    routes: [
      { path: '/', element: element() },
      { path: '/desktop/settings', element: <h1>Settings</h1> },
    ],
  });
  return {
    ...setup,
    onStop,
    onStart,
    rerender: () => void router.navigate('/'),
    toasts: () => services.ui.getState().toasts.map((t) => t.message),
  };
}

const region = () => screen.getByRole('region', { name: 'Your share' });

describe('the bar (docs/m2/03 §10.3)', () => {
  it('shows nothing while nothing is shared', async () => {
    await renderPanel(null);
    expect(screen.queryByRole('region')).not.toBeInTheDocument();
  });

  it('live: the red dot, "You’re live · <label> · <preset> · N watching" and Stop', async () => {
    const r = await renderPanel();
    expect(within(region()).getByText("You're live · Minecraft · Game · 2 watching")).toBeInTheDocument();
    await userEvent.click(within(region()).getByRole('button', { name: 'Stop sharing' }));
    expect(r.onStop).toHaveBeenCalledTimes(1);
    expect(screen.queryByText('No sound captured')).not.toBeInTheDocument();
  });

  it('paused: amber, what it means, and Resume', async () => {
    const r = await renderPanel(shareView({ state: 'paused' }));
    expect(
      within(region()).getByText('Paused · nothing new leaves your PC; friends see "Paused" and hear nothing'),
    ).toBeInTheDocument();
    await userEvent.click(within(region()).getByRole('button', { name: 'Resume' }));
    expect(r.fake.callsOf('share.setPaused')).toEqual([[false]]);
  });

  it('reconnecting: "Reconnecting…"', async () => {
    await renderPanel(shareView({ state: 'reconnecting' }));
    expect(within(region()).getByText('Reconnecting…')).toBeInTheDocument();
  });

  it('unreachable: the media port, and Test connection opens the room’s connection test in the browser', async () => {
    const r = await renderPanel(shareView({ state: 'unreachable', roomId: 'game-night' }));
    expect(within(region()).getByText("Can't reach the server's media port")).toBeInTheDocument();
    await userEvent.click(within(region()).getByRole('button', { name: 'Test connection' }));
    expect(r.opened).toEqual([`${ORIGIN}/r/game-night?conntest=1`]);
  });

  it('the minimized line while the shared window is minimized, and the hints', async () => {
    const r = await renderPanel(shareView({ minimized: true }));
    expect(
      within(region()).getByText('Your shared window is minimized: friends see the last picture'),
    ).toBeInTheDocument();
    act(() => {
      r.fake.emit('share.state', shareView({ hint: { kind: 'upload-limited', approxHeight: 720 } }));
    });
    expect(within(region()).queryByText(/minimized/)).not.toBeInTheDocument();
    expect(within(region()).getByText('Your upload allows about 720p')).toBeInTheDocument();
  });

  it('"Switched to <encoder>" for 5 s on share.notice encoder_fallback', async () => {
    const r = await renderPanel();
    vi.useFakeTimers();
    act(() => {
      r.fake.emit('share.notice', { code: 'encoder_fallback', params: { encoder: 'mf' } });
    });
    expect(within(region()).getByText('Switched to mf')).toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(ENCODER_NOTICE_MS);
    });
    expect(within(region()).queryByText('Switched to mf')).not.toBeInTheDocument();
  });
});

describe('the expander (docs/m2/03 §10.3)', () => {
  it('Optimize for changes the share’s preset; Share sound is there for a share started with sound', async () => {
    const r = await renderPanel();
    await userEvent.click(within(region()).getByRole('button', { name: 'Share settings' }));
    await userEvent.click(within(region()).getByRole('radio', { name: 'Movie' }));
    expect(r.fake.callsOf('share.setPreset')).toEqual([['movie']]);
    await userEvent.click(within(region()).getByRole('switch', { name: 'Share sound' }));
    expect(r.fake.callsOf('share.setAudioEnabled')).toEqual([[false]]);
    expect(within(region()).getByText('Bea')).toBeInTheDocument();
    expect(within(region()).getByText('Someone')).toBeInTheDocument();
  });

  it('no Share sound switch for a share started without sound', async () => {
    await renderPanel(shareView({ withAudio: false, audioOn: false }));
    await userEvent.click(within(region()).getByRole('button', { name: 'Share settings' }));
    expect(within(region()).queryByRole('switch')).not.toBeInTheDocument();
    expect(within(region()).getByText('No sound is shared')).toBeInTheDocument();
  });

  it('Details: Ctrl+Shift+D opens it with the server’s stats, no names; closing stops them', async () => {
    const r = await renderPanel();
    await userEvent.keyboard('{Control>}{Shift>}D{/Shift}{/Control}');
    const details = await waitFor(() => {
      const el = region().querySelector('[data-share-details]');
      if (el === null) throw new Error('no details');
      return el as HTMLElement;
    });
    expect(r.fake.callsOf('share.watchServerStats')).toEqual([[true]]);
    expect(within(details).getByText('nvenc (hardware)')).toBeInTheDocument();
    expect(within(details).getByText('f: 1920 × 1080 @ 60')).toBeInTheDocument();
    expect(within(details).getByText('target 8000 kbit/s, sent 7712 kbit/s')).toBeInTheDocument();
    expect(within(details).getByText('capture to send p50 14 ms, p95 22 ms')).toBeInTheDocument();
    expect(within(details).getByText('Asking the server…')).toBeInTheDocument();
    expect(details).not.toHaveTextContent('Minecraft');
    act(() => {
      r.fake.emit(
        'share.state',
        shareView({
          server: {
            layers: [{ shareId: 's_q7m2k9x4w3bd8c1a', kind: 'video', rid: 'f', bitrate: 7_600_000, lossPct: 0.25 }],
          },
        }),
      );
    });
    expect(within(details).getByText('f: 7600 kbit/s, 0.3 % lost')).toBeInTheDocument();
    await userEvent.keyboard('{Control>}{Shift>}D{/Shift}{/Control}');
    expect(region().querySelector('[data-share-details]')).toBeNull();
    expect(r.fake.callsOf('share.watchServerStats')).toEqual([[true], [false]]);
  });
});

describe('a share the session doesn’t hold', () => {
  it('is shown and stopped all the same (the app’s share before the page adopted it)', async () => {
    const r = await renderPanel(shareView(), { adopted: false });
    expect(within(region()).getByText("You're live · Minecraft · Game · 2 watching")).toBeInTheDocument();
    await userEvent.click(within(region()).getByRole('button', { name: 'Stop sharing' }));
    expect(r.onStop).not.toHaveBeenCalled();
    expect(r.fake.callsOf('share.stop')).toEqual([[]]);
  });
});

describe('a failed share (docs/m2/03 §10.5)', () => {
  it('the driver restarted: its text, Share again starts a new pick, Dismiss clears it', async () => {
    const r = await renderPanel(shareView(), { withStart: true });
    act(() => {
      r.fake.emit(
        'share.state',
        shareView({
          state: 'failed',
          endReason: 'error',
          error: { code: 'engine.encoder_lost', params: { reason: 'device_removed' } },
        }),
      );
    });
    expect(await within(region()).findByRole('alert')).toHaveTextContent(
      'Your graphics driver restarted, so the share stopped.',
    );
    await userEvent.click(within(region()).getByRole('button', { name: 'Share again' }));
    await waitFor(() => {
      expect(r.fake.callsOf('share.pick')).toHaveLength(1);
    });
  });

  it('the encoder stopped: Settings → Sharing; something went wrong: the logs; Dismiss', async () => {
    const r = await renderPanel();
    act(() => {
      r.fake.emit(
        'share.state',
        shareView({ state: 'failed', endReason: 'error', error: { code: 'engine.internal' } }),
      );
    });
    expect(await within(region()).findByRole('alert')).toHaveTextContent(
      "Something went wrong in isshoni's video engine.",
    );
    await userEvent.click(within(region()).getByRole('button', { name: 'Open logs folder' }));
    expect(r.fake.callsOf('app.openLogsFolder')).toEqual([[]]);
    await userEvent.click(within(region()).getByRole('button', { name: 'Dismiss' }));
    expect(screen.queryByRole('region')).not.toBeInTheDocument();
    expect(r.store.getState().phase).toBe('idle');
  });

  it('the window that was shared was closed: the panel says so until it is dismissed', async () => {
    const r = await renderPanel();
    act(() => {
      r.fake.emit('share.state', shareView({ state: 'ended', endReason: 'source-gone' }));
    });
    expect(await within(region()).findByRole('alert')).toHaveTextContent('The window you shared was closed.');
    await userEvent.click(within(region()).getByRole('button', { name: 'Dismiss' }));
    expect(screen.queryByRole('region')).not.toBeInTheDocument();
  });

  it('the server was out of reach for 30 s: "Your share stopped…", and Share again', async () => {
    const r = await renderPanel(shareView(), { withStart: true });
    act(() => {
      r.fake.emit('share.state', shareView({ state: 'ended', endReason: 'server-unreachable' }));
    });
    expect(await within(region()).findByRole('alert')).toHaveTextContent(
      "Your share stopped: isshoni couldn't reach the server for 30 seconds. Check your connection and share again.",
    );
    await userEvent.click(within(region()).getByRole('button', { name: 'Share again' }));
    await waitFor(() => {
      expect(r.fake.callsOf('share.pick')).toHaveLength(1);
    });
  });

  it('a pick that timed out (pick_expired again): it says so, and Pick again starts a new pick', async () => {
    const r = await renderPanel(null, { withStart: true });
    act(() => {
      r.store.setState({ phase: 'failed', error: new BridgeError('pick_expired', { reason: 'pick_expired' }) });
    });
    expect(await within(region()).findByRole('alert')).toHaveTextContent(
      'That pick timed out. Pick what to share again.',
    );
    await userEvent.click(within(region()).getByRole('button', { name: 'Pick again' }));
    await waitFor(() => {
      expect(r.onStart).toHaveBeenCalledTimes(1);
    });
    expect(r.fake.callsOf('share.pick')).toHaveLength(1);
  });

  it('Share again whose pick fails before any share: a toast says why, as the panel goes', async () => {
    const r = await renderPanel(shareView(), { withStart: true });
    r.fake.answers['share.pick'] = () => Promise.reject(new BridgeError('engine.invalid', { reason: 'self_window' }));
    act(() => {
      r.fake.emit(
        'share.state',
        shareView({
          state: 'failed',
          endReason: 'error',
          error: { code: 'engine.encoder_lost', params: { reason: 'device_removed' } },
        }),
      );
    });
    await userEvent.click(await within(region()).findByRole('button', { name: 'Share again' }));
    await waitFor(() => {
      expect(r.toasts()).toEqual(['Pick something other than isshoni.']);
    });
    expect(screen.queryByRole('region')).not.toBeInTheDocument();
    expect(r.onStart).not.toHaveBeenCalled();
  });

  it('a share that ended normally leaves no panel', async () => {
    const r = await renderPanel();
    act(() => {
      r.fake.emit('share.state', shareView({ state: 'ended', endReason: 'user' }));
    });
    await waitFor(() => {
      expect(screen.queryByRole('region')).not.toBeInTheDocument();
    });
  });
});
