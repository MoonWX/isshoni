// The sharer's panel (05 §13.7): a view of shareStore's second half with the controls of the local share.
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi, type Mock } from 'vitest';

import { AppProviders } from '../app/App';
import type { AppServices } from '../app/context';
import { LocalError } from '../lib/errors';
import type { ActiveShare, PickedSource } from '../platform/types';
import { ProtocolError } from '../protocol/errors';
import { makeError } from '../protocol/testing';
import type { Preset } from '../protocol/types.gen';
import { createTestServices, renderWithApp } from '../test/render';
import { ShareEndedError } from './BrowserShare';
import { SharePanel, WATCHERS_ANNOUNCE_MS, type SharePanelProps } from './SharePanel';
import { createShareStore, type ShareStore } from './shareStore';
import { fakePick } from './testing/fakeCapture';
import { shareParams } from './testing/publish';
import type { ShareWatcher } from './watchers';

interface FakeShare extends ActiveShare {
  setPreset: Mock<ActiveShare['setPreset']>;
  setAudioEnabled: Mock<ActiveShare['setAudioEnabled']>;
}

function fakeShare(src: PickedSource = fakePick('window', true)): FakeShare {
  return {
    shareId: 's_1',
    kind: src.kind,
    preview: src.preview,
    state: 'live',
    params: shareParams('s_1'),
    on: () => () => undefined,
    setPreset: vi.fn<ActiveShare['setPreset']>(() => Promise.resolve()),
    setPaused: () => Promise.reject(new Error('not in M1')),
    setAudioEnabled: vi.fn<ActiveShare['setAudioEnabled']>(() => Promise.resolve()),
    stop: () => Promise.resolve(),
    stats: () => Promise.resolve(null),
  };
}

const WATCHERS: ShareWatcher[] = [
  { userId: 'u_bea', name: 'Bea' },
  { userId: 'u_cy', name: 'Cy' },
  { userId: 'u_gone', name: '' },
];

interface Setup extends Partial<SharePanelProps> {
  /** How far the share is; default live. */
  phase?: 'starting' | 'live' | 'reconnecting' | 'stopping';
  src?: PickedSource;
  preset?: Preset;
  withAudio?: boolean;
}

/** A store with a share of a window (Movie preset unless told otherwise) in the given phase. */
function sharingStore({ phase = 'live', src = fakePick('window', true), preset = 'movie', withAudio }: Setup = {}) {
  const store = createShareStore();
  store.getState().publishing({
    picked: { kind: src.kind, audioScope: src.audioScope, warning: src.warning },
    preset,
    withAudio: withAudio ?? src.audioScope !== 'none',
    params: shareParams('s_1'),
  });
  if (phase !== 'starting') store.getState().advance('live');
  if (phase === 'reconnecting' || phase === 'stopping') store.getState().advance(phase);
  return store;
}

function renderPanel(setup: Setup = {}) {
  const src = setup.src ?? fakePick('window', true);
  const store: ShareStore = setup.store ?? sharingStore({ ...setup, src });
  const share = setup.share === undefined ? fakeShare(src) : setup.share;
  const onStop = vi.fn();
  const services = createTestServices();
  const props: SharePanelProps = {
    share,
    onStop,
    watchers: setup.watchers,
    onTestConnection: setup.onTestConnection,
    store,
  };
  const result = renderWithApp(<SharePanel {...props} />, { services });
  const rerender = (next: Partial<SharePanelProps>): void => {
    result.rerender(<SharePanelIn services={services} {...props} {...next} />);
  };
  const toasts = () => services.ui.getState().toasts.map((t) => ({ kind: t.kind, message: t.message }));
  return { ...result, store, share, onStop, services, toasts, rerender };
}

/** renderWithApp's providers again, for rerenders with other props. */
function SharePanelIn({ services, ...props }: SharePanelProps & { services: AppServices }) {
  return (
    <AppProviders services={services}>
      <SharePanel {...props} />
    </AppProviders>
  );
}

const panel = (): HTMLElement => screen.getByRole('region', { name: 'Your share' });
const openDetails = async (): Promise<void> => {
  await userEvent.click(screen.getByRole('button', { name: 'Share settings' }));
};

afterEach(() => {
  vi.useRealTimers();
});

describe('SharePanel', () => {
  it.each(['idle', 'picking', 'confirming'] as const)('shows nothing while the share store is %s', (phase) => {
    const store = createShareStore();
    store.setState({ phase });
    renderPanel({ store, share: null });
    expect(screen.queryByRole('region', { name: 'Your share' })).not.toBeInTheDocument();
  });

  it('live: "You’re live · Window · Movie · 3 watching" and Stop', async () => {
    const { onStop } = renderPanel({ watchers: WATCHERS });
    expect(panel()).toHaveTextContent("You're live · Window · Movie · 3 watching");
    await userEvent.click(screen.getByRole('button', { name: 'Stop sharing' }));
    expect(onStop).toHaveBeenCalledOnce();
  });

  it.each([
    ['browser', 'auto', "You're live · Tab · Auto · 0 watching"],
    ['monitor', 'text', "You're live · Screen · Text · 0 watching"],
  ] as const)('names what is shared by kind and preset (%s, %s), never by a window title', (surface, preset, text) => {
    renderPanel({ src: fakePick(surface, false), preset });
    expect(panel()).toHaveTextContent(text);
    expect(panel()).not.toHaveTextContent('Secret Window Title');
  });

  it('counts one watcher in the singular', () => {
    renderPanel({ watchers: WATCHERS.slice(0, 1) });
    expect(panel()).toHaveTextContent('1 watching');
  });

  it('starting: says so, with Stop, and without the settings', async () => {
    const { onStop } = renderPanel({ phase: 'starting' });
    expect(panel()).toHaveTextContent('Starting your share…');
    expect(screen.queryByRole('button', { name: 'Share settings' })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Stop sharing' }));
    expect(onStop).toHaveBeenCalledOnce();
  });

  it('reconnecting: says so; when the media port can’t be reached, that, with [Test my connection]', async () => {
    const onTestConnection = vi.fn();
    const { store } = renderPanel({ phase: 'reconnecting', onTestConnection });
    expect(panel()).toHaveTextContent('Reconnecting your share…');
    expect(screen.queryByRole('button', { name: 'Test my connection' })).not.toBeInTheDocument();
    act(() => {
      store.getState().report({ unreachable: true });
    });
    expect(panel()).toHaveTextContent("Can't reach the server's media port. isshoni keeps trying.");
    await userEvent.click(screen.getByRole('button', { name: 'Test my connection' }));
    expect(onTestConnection).toHaveBeenCalledOnce();
  });

  it('stopping: Stop is busy and does not stop twice', async () => {
    const { onStop } = renderPanel({ phase: 'stopping' });
    expect(panel()).toHaveTextContent('Stopping your share…');
    const stop = screen.getByRole('button', { name: 'Stop sharing' });
    expect(stop).toHaveAttribute('aria-busy', 'true');
    await userEvent.click(stop);
    expect(onStop).not.toHaveBeenCalled();
  });

  it('follows the store: live → stopping → gone', () => {
    const { store } = renderPanel();
    act(() => {
      store.getState().advance('stopping');
    });
    expect(panel()).toHaveTextContent('Stopping your share…');
    act(() => {
      store.getState().finish();
    });
    expect(screen.queryByRole('region', { name: 'Your share' })).not.toBeInTheDocument();
  });
});

describe('SharePanel: the settings behind the expander', () => {
  it('is closed at first and opens on the button', async () => {
    renderPanel({ watchers: WATCHERS });
    const toggle = screen.getByRole('button', { name: 'Share settings' });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('radio')).not.toBeInTheDocument();
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByRole('group', { name: 'What are you sharing?' })).toBeInTheDocument();
    await userEvent.click(toggle);
    expect(screen.queryByRole('radio')).not.toBeInTheDocument();
  });

  it('the preset picker shows the share’s preset and changes it on the share; the new one is remembered', async () => {
    const share = fakeShare();
    const { services, store } = renderPanel({ share, preset: 'movie' });
    await openDetails();
    expect(
      screen.getAllByRole('radio').map((r) => [(r as HTMLInputElement).value, (r as HTMLInputElement).checked]),
    ).toEqual([
      ['auto', false],
      ['game', false],
      ['movie', true],
      ['text', false],
    ]);
    await userEvent.click(screen.getByRole('radio', { name: 'Text' }));
    expect(share.setPreset).toHaveBeenCalledExactlyOnceWith('text');
    await waitFor(() => {
      expect(services.prefs.getState().preset).toBe('text');
    });
    // The store is the share's to update (BrowserShare.setPreset does); the panel follows it.
    act(() => {
      store.getState().report({ preset: 'text' });
    });
    expect(screen.getByRole('radio', { name: 'Text' })).toBeChecked();
    expect(panel()).toHaveTextContent("You're live · Window · Text");
  });

  it('a preset the server refuses: the error as a toast, the picker as it was', async () => {
    const share = fakeShare();
    share.setPreset.mockRejectedValueOnce(ProtocolError.fromWire(makeError('share_not_found', 'request')));
    const { toasts, services } = renderPanel({ share, preset: 'movie' });
    await openDetails();
    await userEvent.click(screen.getByRole('radio', { name: 'Game' }));
    await waitFor(() => {
      expect(toasts()).toHaveLength(1);
    });
    expect(toasts()[0]?.kind).toBe('error');
    expect(screen.getByRole('radio', { name: 'Movie' })).toBeChecked();
    expect(services.prefs.getState().preset).toBe('auto');
  });

  it('the sound switch turns the shared sound off and on', async () => {
    const share = fakeShare();
    const { store } = renderPanel({ share });
    await openDetails();
    const sound = screen.getByRole('switch', { name: 'Share sound' });
    expect(sound).toBeChecked();
    await userEvent.click(sound);
    expect(share.setAudioEnabled).toHaveBeenCalledExactlyOnceWith(false);
    act(() => {
      store.getState().report({ soundOn: false });
    });
    expect(sound).not.toBeChecked();
    await userEvent.click(sound);
    expect(share.setAudioEnabled).toHaveBeenLastCalledWith(true);
  });

  it('shows the level meter for a share with sound', async () => {
    renderPanel();
    await openDetails();
    expect(screen.getByRole('meter', { name: 'Sound level' })).toBeInTheDocument();
  });

  it.each([
    ['window', "This window's sound isn't shared (needs Windows 11 or macOS 14.2+ and a current Chrome/Edge)"],
    ['browser', "Tick 'Also share tab audio' to include sound"],
    ['monitor', 'No sound is shared'],
  ] as const)('a %s picked without sound: the note of 05 §13.3 instead of the switch', async (surface, note) => {
    renderPanel({ src: fakePick(surface, false) });
    await openDetails();
    expect(screen.getByText(note)).toBeInTheDocument();
    expect(screen.queryByRole('switch')).not.toBeInTheDocument();
    expect(screen.queryByRole('meter')).not.toBeInTheDocument();
  });

  it('a whole screen shared "without sound": no switch, "No sound is shared"', async () => {
    renderPanel({ src: fakePick('monitor', true), withAudio: false });
    await openDetails();
    expect(screen.getByText('No sound is shared')).toBeInTheDocument();
    expect(screen.queryByRole('switch')).not.toBeInTheDocument();
  });

  it('lists who is watching, by name', async () => {
    renderPanel({ watchers: WATCHERS });
    await openDetails();
    const list = screen.getByRole('list');
    expect(
      within(list)
        .getAllByRole('listitem')
        .map((li) => li.textContent),
    ).toEqual(['Bea', 'Cy', 'Someone']);
  });

  it('says when nobody is watching yet', async () => {
    renderPanel();
    await openDetails();
    expect(screen.getByText('Nobody is watching yet.')).toBeInTheDocument();
    expect(screen.queryByRole('list')).not.toBeInTheDocument();
  });

  it('without the share object the switches are there but can’t be used', async () => {
    renderPanel({ share: null });
    await openDetails();
    expect(screen.getByRole('radio', { name: 'Auto' })).toBeDisabled();
    expect(screen.getByRole('switch', { name: 'Share sound' })).toBeDisabled();
  });
});

describe('SharePanel: hints (05 §13.7)', () => {
  it('shows the upload hint in the bar, and takes it away again', () => {
    const { store } = renderPanel();
    act(() => {
      store.getState().report({ hint: { kind: 'upload-limited', approxHeight: 720 } });
    });
    expect(screen.getByText('Your upload allows about 720p')).toBeInTheDocument();
    act(() => {
      store.getState().report({ hint: null });
    });
    expect(screen.queryByText(/Your upload allows/)).not.toBeInTheDocument();
  });

  it('shows the CPU hint', () => {
    const { store } = renderPanel();
    act(() => {
      store.getState().report({ hint: { kind: 'cpu-limited' } });
    });
    expect(
      screen.getByText('Your computer is struggling to encode. Try the Text preset or share a smaller window.'),
    ).toBeInTheDocument();
  });
});

describe('SharePanel: a failed share', () => {
  const failedStore = (error: unknown): ShareStore => {
    const store = sharingStore();
    store.getState().finish({ error });
    return store;
  };

  it('shows why, until it is dismissed', async () => {
    const store = failedStore(new ShareEndedError('server', 'disconnected'));
    renderPanel({ store, share: null });
    expect(within(panel()).getByRole('alert')).toHaveTextContent('Sharing stopped by the server');
    expect(screen.queryByRole('button', { name: 'Stop sharing' })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(store.getState().phase).toBe('idle');
    expect(screen.queryByRole('region', { name: 'Your share' })).not.toBeInTheDocument();
  });

  it('media_timeout: "no video reached the server", with [Test my connection]', async () => {
    const onTestConnection = vi.fn();
    renderPanel({
      store: failedStore(new ShareEndedError('media_timeout', 'media_timeout')),
      share: null,
      onTestConnection,
    });
    expect(panel()).toHaveTextContent('Sharing stopped: no video reached the server');
    await userEvent.click(screen.getByRole('button', { name: 'Test my connection' }));
    expect(onTestConnection).toHaveBeenCalledOnce();
  });

  it('other failures have their own text and no connection test', () => {
    renderPanel({ store: failedStore(new LocalError('webrtc_failed')), share: null, onTestConnection: vi.fn() });
    expect(panel()).toHaveTextContent("Couldn't set up the video connection.");
    expect(screen.queryByRole('button', { name: 'Test my connection' })).not.toBeInTheDocument();
  });
});

describe('SharePanel: the page around it', () => {
  it('announces "3 watching" politely once the count has been the same for 5 s, not the count it went live with', () => {
    vi.useFakeTimers();
    const { services, rerender } = renderPanel({ watchers: WATCHERS.slice(0, 1) });
    const announce = vi.spyOn(services.ui.getState(), 'announce');
    act(() => {
      vi.advanceTimersByTime(WATCHERS_ANNOUNCE_MS * 2);
    });
    expect(announce).not.toHaveBeenCalled();

    // Two friends arrive one after the other: one announcement, for the count that stayed.
    rerender({ watchers: WATCHERS.slice(0, 2) });
    act(() => {
      vi.advanceTimersByTime(WATCHERS_ANNOUNCE_MS - 1);
    });
    rerender({ watchers: WATCHERS });
    act(() => {
      vi.advanceTimersByTime(WATCHERS_ANNOUNCE_MS - 1);
    });
    expect(announce).not.toHaveBeenCalled();
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(announce).toHaveBeenCalledExactlyOnceWith('3 watching');

    // Back to a count that was announced before the change settled: nothing new to say.
    rerender({ watchers: WATCHERS.slice(0, 2) });
    rerender({ watchers: WATCHERS });
    act(() => {
      vi.advanceTimersByTime(WATCHERS_ANNOUNCE_MS);
    });
    expect(announce).toHaveBeenCalledOnce();
  });

  it('counts as the store’s panel while mounted, and links the share state to uiStore', () => {
    const { store, services, unmount } = renderPanel();
    expect(store.getState().panels).toBe(1);
    expect(services.ui.getState().sharing).toBe(true);
    unmount();
    expect(store.getState().panels).toBe(0);
    // The link stays: the share goes on when the room page is left.
    expect(services.ui.getState().sharing).toBe(true);
    store.getState().finish();
    expect(services.ui.getState().sharing).toBe(false);
  });
});
