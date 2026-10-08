// The share state in the app-wide UI: uiStore.sharing and the update pill (05 §16.2), the "stopped elsewhere" toast
// (05 §13.1), and the texts of a failed share.
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { createMemoryRouter } from 'react-router';
import { afterEach, describe, expect, it } from 'vitest';

import { App } from '../app/App';
import { createUiStore } from '../app/uiStore';
import { LocalError } from '../lib/errors';
import { ProtocolError } from '../protocol/errors';
import { SignalClient } from '../protocol/signal-client';
import { FakeSignalServer, makeError } from '../protocol/testing';
import type { ShareStop } from '../protocol/types.gen';
import { FAKE_AUDIO_CODECS, FAKE_VIDEO_CODECS } from '../test/FakeRTCPeerConnection';
import { createTestPlatform } from '../test/platform';
import { createTestServices } from '../test/render';
import { ShareEndedError } from './BrowserShare';
import { BrowserSharing } from './BrowserSharing';
import { ShareButton } from './ShareButton';
import { createShareStore, shareStore, type SharePhase } from './shareStore';
import { linkShareUi, shareErrorMessage } from './shareUi';
import { fakePick } from './testing/fakeCapture';
import { FakeShareHub, shareInfo, shareParams } from './testing/publish';

const WINDOW = { kind: 'window', audioScope: 'window', warning: null } as const;

describe('linkShareUi', () => {
  it('uiStore.sharing follows the share state: true from starting to stopping', () => {
    const store = createShareStore();
    const ui = createUiStore();
    linkShareUi(store, ui);
    expect(ui.getState().sharing).toBe(false);

    const seen: [SharePhase, boolean][] = [];
    const note = (): void => {
      seen.push([store.getState().phase, ui.getState().sharing]);
    };
    store.setState({ phase: 'picking' });
    note();
    store.setState({ phase: 'confirming' });
    note();
    store.getState().publishing({ picked: WINDOW, preset: 'auto', withAudio: true, params: shareParams('s_1') });
    note();
    store.getState().advance('live');
    note();
    store.getState().advance('reconnecting');
    note();
    store.getState().advance('stopping');
    note();
    store.getState().finish({ error: new Error('x') });
    note();
    store.getState().dismiss();
    note();
    expect(seen).toEqual([
      ['picking', false],
      ['confirming', false],
      ['starting', true],
      ['live', true],
      ['reconnecting', true],
      ['stopping', true],
      ['failed', false],
      ['idle', false],
    ]);
  });

  it('takes over a share that is already under way when the link is made', () => {
    const store = createShareStore();
    store.getState().publishing({ picked: WINDOW, preset: 'auto', withAudio: true, params: shareParams('s_1') });
    const ui = createUiStore();
    linkShareUi(store, ui);
    expect(ui.getState().sharing).toBe(true);
  });

  it('a share the server ended without a failure becomes a toast, once per notice', () => {
    const store = createShareStore();
    const ui = createUiStore();
    linkShareUi(store, ui);
    linkShareUi(store, ui); // a second Share button mounting: still one link
    const toasts = (): string[] => ui.getState().toasts.map((t) => `${t.kind}: ${t.message}`);

    store.getState().publishing({ picked: WINDOW, preset: 'auto', withAudio: true, params: shareParams('s_1') });
    store.getState().finish({ notice: 'elsewhere' });
    expect(toasts()).toEqual(['info: Sharing was stopped from another tab or device']);
    // Other changes of the store don't repeat it.
    store.setState({ preset: 'game' });
    expect(toasts()).toHaveLength(1);

    store.getState().publishing({ picked: WINDOW, preset: 'auto', withAudio: true, params: shareParams('s_2') });
    expect(toasts()).toHaveLength(1);
    store.getState().finish({ notice: 'elsewhere' });
    expect(toasts()).toHaveLength(2);
    // A plain stop is nothing to tell.
    store.getState().publishing({ picked: WINDOW, preset: 'auto', withAudio: true, params: shareParams('s_3') });
    store.getState().finish();
    expect(toasts()).toHaveLength(2);
  });
});

describe('shareErrorMessage', () => {
  it('has the texts of a share the server ended (05 §13.1)', () => {
    expect(shareErrorMessage(new ShareEndedError('media_timeout', 'media_timeout'))).toBe(
      'Sharing stopped: no video reached the server',
    );
    expect(shareErrorMessage(new ShareEndedError('server', 'disconnected'))).toBe('Sharing stopped by the server');
    expect(shareErrorMessage(new ShareEndedError('server'))).toBe('Sharing stopped by the server');
  });

  it('and the error’s own text otherwise', () => {
    expect(shareErrorMessage(new LocalError('h264_unavailable'))).toBe(
      "This browser can't send H.264 video. Use Chrome or Edge.",
    );
    expect(shareErrorMessage(new LocalError('webrtc_failed'))).toBe("Couldn't set up the video connection.");
    const wire = ProtocolError.fromWire(makeError('codec_not_supported', 'share', { shareId: 's_1' }));
    expect(shareErrorMessage(wire)).not.toMatch(/unknown/i);
  });
});

// The shell as boot builds it: <App> with the pill it mounts itself, a page with a Share button, the page's own
// share store and a real in-page sharer on a fake server. Nothing here is told that a share is under way; it has
// to find out from the share state.
describe('the shell’s update pill while this page shares (05 §16.2)', () => {
  let server: FakeSignalServer | undefined;
  let signal: SignalClient | undefined;

  afterEach(() => {
    signal?.stop();
    server?.uninstall();
    signal = undefined;
    server = undefined;
  });

  it('offers no Reload from the moment the share starts until it has stopped', async () => {
    server = FakeSignalServer.install();
    const hub = new FakeShareHub(server);
    const src = fakePick('window', true);
    const sharing = new BrowserSharing({
      capture: () => Promise.resolve(src),
      platform: createTestPlatform(),
      capabilities: (kind) => (kind === 'video' ? FAKE_VIDEO_CODECS : FAKE_AUDIO_CODECS),
    });
    const platform = createTestPlatform({ sharing });
    const client = new SignalClient({
      url: platform.signaling().url,
      client: platform.client,
      role: 'full',
      caps: () => platform.capsNow(),
    });
    signal = client;
    client.start();
    await waitFor(() => {
      expect(client.state).toBe('ready');
    });

    const services = createTestServices({ platform });
    let started: Awaited<ReturnType<BrowserSharing['start']>> | undefined;
    const router = createMemoryRouter([
      {
        path: '*',
        element: (
          <ShareButton
            onStart={async (picked, opts) => {
              started = await sharing.start(picked, opts, { signal: client, roomId: 'lounge' });
            }}
          />
        ),
      },
    ]);
    render(<App services={services} router={router} />);

    // An update is ready before anything is shared: the shell offers the reload.
    act(() => {
      services.ui.getState().setUpdateReady(true);
    });
    const pill = screen.getByRole('region', { name: 'App update' });
    expect(within(pill).getByRole('button', { name: 'Reload' })).toBeInTheDocument();

    // Share → the sheet → Share: the picker "returns" a window, and the share starts.
    await userEvent.click(screen.getByRole('button', { name: 'Share' }));
    await userEvent.click(
      within(screen.getByRole('dialog', { name: 'Share your screen' })).getByRole('button', { name: 'Share' }),
    );
    await waitFor(() => {
      expect(shareStore.getState().phase).toBe('starting');
    });
    expect(services.ui.getState().sharing).toBe(true);
    expect(within(pill).queryByRole('button', { name: 'Reload' })).not.toBeInTheDocument();
    expect(pill).toHaveTextContent('Update ready. Reload after you stop sharing.');

    // Live: still none.
    hub.answer();
    hub.roomState('lounge', [shareInfo('s_1')]);
    await waitFor(() => {
      expect(shareStore.getState().phase).toBe('live');
    });
    expect(within(pill).queryByRole('button', { name: 'Reload' })).not.toBeInTheDocument();

    // The share stops: the reload is offered again.
    await act(async () => {
      await started?.stop();
    });
    expect(hub.requests<ShareStop>('share.stop')).toEqual([{ shareId: 's_1' }]);
    expect(shareStore.getState().phase).toBe('idle');
    expect(services.ui.getState().sharing).toBe(false);
    expect(within(pill).getByRole('button', { name: 'Reload' })).toBeInTheDocument();
    expect(pill).toHaveTextContent('Update ready');
  });
});
