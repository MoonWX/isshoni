// The share state in the app-wide UI: uiStore.sharing and the update pill (05 §16.2), the "stopped elsewhere" toast
// (05 §13.1), the "Can't connect media" screen when the pub PC gave up (05 §9), and the texts of a failed share.
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
import { PubNegotiationFailedError, ShareEndedError } from './BrowserShare';
import { BrowserSharing } from './BrowserSharing';
import { ShareButton } from './ShareButton';
import { createShareStore, shareStore, type SharePhase } from './shareStore';
import { linkShareUi, shareErrorMessage } from './shareUi';
import { fakePick } from './testing/fakeCapture';
import { FakeShareHub, pubOffers, shareInfo, shareParams } from './testing/publish';

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

  it('only a share that ended because the pub PC gave up becomes the "Can’t connect media" screen', () => {
    const store = createShareStore();
    const ui = createUiStore();
    linkShareUi(store, ui);
    const publish = (): void => {
      store.getState().publishing({ picked: WINDOW, preset: 'auto', withAudio: true, params: shareParams('s_1') });
    };

    // A start that failed once and a share the server ended are failed shares: the share panel shows them, and
    // Share can be clicked again.
    for (const error of [new LocalError('webrtc_failed'), new ShareEndedError('media_timeout', 'media_timeout')]) {
      publish();
      store.getState().finish({ error });
      expect(store.getState().phase).toBe('failed');
      expect(ui.getState().screen).toBeNull();
    }
    publish();
    store.getState().finish();
    expect(ui.getState().screen).toBeNull();

    publish();
    store.getState().finish({ error: new PubNegotiationFailedError() });
    expect(store.getState().phase).toBe('failed');
    expect(ui.getState().screen).toEqual({ kind: 'fatal', reason: 'media' });
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
describe('the shell while this page shares', () => {
  let server: FakeSignalServer | undefined;
  let signal: SignalClient | undefined;

  afterEach(() => {
    signal?.stop();
    server?.uninstall();
    signal = undefined;
    server = undefined;
    // The page's store is the module's: a share that failed in one test must not be the next one's.
    shareStore.getState().dismiss();
  });

  async function mountShell() {
    const fake = FakeSignalServer.install();
    server = fake;
    const hub = new FakeShareHub(fake);
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
    const started: Awaited<ReturnType<BrowserSharing['start']>>[] = [];
    const router = createMemoryRouter([
      {
        path: '*',
        element: (
          <ShareButton
            onStart={async (picked, opts) => {
              started.push(await sharing.start(picked, opts, { signal: client, roomId: 'lounge' }));
            }}
          />
        ),
      },
    ]);
    render(<App services={services} router={router} />);
    return { server: fake, hub, services, started };
  }

  /** Share → the sheet → Share: the picker "returns" a window, and the share starts. */
  async function shareAWindow(): Promise<void> {
    await userEvent.click(screen.getByRole('button', { name: 'Share' }));
    await userEvent.click(
      within(screen.getByRole('dialog', { name: 'Share your screen' })).getByRole('button', { name: 'Share' }),
    );
    await waitFor(() => {
      expect(shareStore.getState().phase).toBe('starting');
    });
  }

  it('the update pill offers no Reload from the moment the share starts until it has stopped (05 §16.2)', async () => {
    const { hub, services, started } = await mountShell();

    // An update is ready before anything is shared: the shell offers the reload.
    act(() => {
      services.ui.getState().setUpdateReady(true);
    });
    const pill = screen.getByRole('region', { name: 'App update' });
    expect(within(pill).getByRole('button', { name: 'Reload' })).toBeInTheDocument();

    await shareAWindow();
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
      await started[0]?.stop();
    });
    expect(hub.requests<ShareStop>('share.stop')).toEqual([{ shareId: 's_1' }]);
    expect(shareStore.getState().phase).toBe('idle');
    expect(services.ui.getState().sharing).toBe(false);
    expect(within(pill).getByRole('button', { name: 'Reload' })).toBeInTheDocument();
    expect(pill).toHaveTextContent('Update ready');
  });

  it('sdp_invalid for the pub PC twice within a minute: "Can’t connect media" with Reload (05 §9)', async () => {
    const { server: fake, hub, services } = await mountShell();
    await shareAWindow();
    await waitFor(() => {
      expect(pubOffers(fake)).toHaveLength(1);
    });

    // The first one: the pub PC is rebuilt once (gen 2), and the share goes on.
    act(() => {
      fake.error(makeError('sdp_invalid', 'pc', { pc: 'pub', gen: 1, neg: 1 }));
    });
    await waitFor(() => {
      expect(pubOffers(fake).at(-1)).toMatchObject({ gen: 2, neg: 1 });
    });
    expect(shareStore.getState().phase).toBe('starting');
    expect(services.ui.getState().screen).toBeNull();

    // The second one ends it: not a failed share with Dismiss, but the app's screen with Reload.
    act(() => {
      fake.error(makeError('sdp_invalid', 'pc', { pc: 'pub', gen: 2, neg: 1 }));
    });
    await waitFor(() => {
      expect(services.ui.getState().screen).toEqual({ kind: 'fatal', reason: 'media' });
    });
    expect(screen.getByRole('heading', { name: "Can't connect media" })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Reload' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Share' })).not.toBeInTheDocument();
    // The share itself is over, on the server too.
    expect(hub.requests<ShareStop>('share.stop')).toEqual([{ shareId: 's_1' }]);
    expect(services.ui.getState().sharing).toBe(false);
  });
});
