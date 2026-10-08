// The Share button with the whole pick flow behind it: sheet → picker → (warning) → start (05 §13.1, §13.3, §13.8).
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi, type Mock } from 'vitest';

import { LocalError, NotImplementedError } from '../lib/errors';
import type { PickedSource } from '../platform/types';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderRoute } from '../test/render';
import { NOTE_TOAST_MS, ShareButton, ShareUnavailableNote, type ShareButtonProps } from './ShareButton';
import { createShareStore, type StartShare } from './shareStore';
import { deferred, fakePick, fakeSharing, type FakeSharing } from './testing/fakeCapture';

const WARNING = 'Share all sound on this computer?';

interface Setup extends Partial<ShareButtonProps> {
  sharing?: FakeSharing | null;
  /** A second Share button on the same page (the room's empty state). */
  second?: boolean;
}

function renderButton({ sharing = fakeSharing(), second = false, ...props }: Setup = {}) {
  const store = props.store ?? createShareStore();
  const onStart: Mock<StartShare> = vi.fn<StartShare>(() => Promise.resolve());
  const services = createTestServices({ platform: createTestPlatform({ sharing }) });
  const result = renderRoute({
    services,
    routes: [
      {
        path: '/',
        element: (
          <>
            <ShareButton onStart={onStart} store={store} {...props} />
            {second && <ShareButton onStart={onStart} store={store} label="Share your screen" {...props} />}
          </>
        ),
      },
      { path: '/download', element: <h1>Download page</h1> },
    ],
  });
  const toasts = () => services.ui.getState().toasts.map((t) => ({ kind: t.kind, message: t.message }));
  return { ...result, store, onStart, sharing, services, toasts };
}

/** Clicks the Share button, then Share in the sheet. */
async function shareFromSheet(buttonName = 'Share'): Promise<void> {
  await userEvent.click(screen.getByRole('button', { name: buttonName }));
  const sheet = screen.getByRole('dialog', { name: 'Share your screen' });
  await userEvent.click(within(sheet).getByRole('button', { name: 'Share' }));
}

describe('ShareButton', () => {
  it('appears wherever the platform can share, and opens the sheet', async () => {
    renderButton();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Share' }));
    expect(screen.getByRole('dialog', { name: 'Share your screen' })).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('is absent where the platform cannot share (phones and tablets, 05 §13.8)', () => {
    renderButton({ sharing: null });
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });

  it('ShareUnavailableNote says what phones get instead', () => {
    renderRoute({ routes: [{ path: '/', element: <ShareUnavailableNote /> }] });
    expect(screen.getByText('Sharing from phones needs the app (coming later)')).toBeInTheDocument();
  });

  it('takes its label and look from the caller (the empty state\'s "Share your screen")', () => {
    renderButton({ label: 'Share your screen', variant: 'secondary' });
    expect(screen.getByRole('button', { name: 'Share your screen' })).toBeInTheDocument();
  });

  it('a window with its sound goes straight to start: no warning, no note', async () => {
    const src = fakePick('window', true);
    const { onStart, sharing, store, toasts } = renderButton({ sharing: fakeSharing(src) });
    await shareFromSheet();
    await waitFor(() => {
      expect(onStart).toHaveBeenCalledExactlyOnceWith(src, { preset: 'auto', withAudio: true });
    });
    expect(sharing?.pick).toHaveBeenCalledExactlyOnceWith({ preset: 'auto' });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(store.getState()).toMatchObject({ phase: 'starting', picked: { kind: 'window', audioScope: 'window' } });
    expect(toasts()).toEqual([]);
    expect(src.release).not.toHaveBeenCalled();
  });

  it('closes the sheet and blocks a second share while the picker is open', async () => {
    const picking = deferred<PickedSource | null>();
    const sharing = fakeSharing();
    sharing.pick.mockReturnValueOnce(picking.promise);
    const { store } = renderButton({ sharing });
    await shareFromSheet();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(store.getState().phase).toBe('picking');
    expect(screen.getByRole('button', { name: 'Share' })).toBeDisabled();
    picking.resolve(null);
    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'Share' })).toBeEnabled();
    });
    expect(store.getState().phase).toBe('idle');
  });

  it.each([
    ['window', "This window's sound isn't shared (needs Windows 11 or macOS 14.2+ and a current Chrome/Edge)"],
    ['browser', "Tick 'Also share tab audio' to include sound"],
    ['monitor', 'No sound is shared'],
  ] as const)('a %s pick without sound starts, with the note of 05 §13.3', async (surface, note) => {
    const src = fakePick(surface, false);
    const { onStart, toasts, services } = renderButton({ sharing: fakeSharing(src) });
    await shareFromSheet();
    await waitFor(() => {
      expect(toasts()).toEqual([{ kind: 'info', message: note }]);
    });
    expect(onStart).toHaveBeenCalledExactlyOnceWith(src, { preset: 'auto', withAudio: false });
    expect(services.ui.getState().toasts[0]?.durationMs).toBe(NOTE_TOAST_MS);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('a capture that fails shows its message and returns to idle', async () => {
    const { onStart, store, toasts } = renderButton({ sharing: fakeSharing(new LocalError('capture_failed')) });
    await shareFromSheet();
    await waitFor(() => {
      expect(toasts()).toEqual([{ kind: 'error', message: "Couldn't capture that. Another app may be blocking it." }]);
    });
    expect(onStart).not.toHaveBeenCalled();
    expect(store.getState().phase).toBe('idle');
    expect(screen.getByRole('button', { name: 'Share' })).toBeEnabled();
  });

  it('a start that fails shows its message, releases the capture, and leaves Share usable', async () => {
    const src = fakePick('window', true);
    const { onStart, store, toasts } = renderButton({ sharing: fakeSharing(src) });
    // What platform.sharing.start does until S46.
    onStart.mockRejectedValueOnce(new NotImplementedError('sharing.start', 'S46'));
    await shareFromSheet();
    await waitFor(() => {
      expect(toasts()).toEqual([{ kind: 'error', message: 'Something went wrong (unknown).' }]);
    });
    expect(store.getState().phase).toBe('failed');
    expect(src.release).toHaveBeenCalledOnce();
    expect(screen.getByRole('button', { name: 'Share' })).toBeEnabled();
  });

  it.each(['starting', 'live', 'reconnecting', 'stopping'] as const)(
    'is disabled while this page already shares (%s): one share per page in M1',
    (phase) => {
      const store = createShareStore();
      store.setState({ phase });
      renderButton({ store });
      expect(screen.getByRole('button', { name: 'Share' })).toBeDisabled();
    },
  );

  it('passes "already sharing from another tab or device" to the sheet', async () => {
    const onStop = vi.fn(() => Promise.resolve());
    renderButton({ elsewhere: { onStop } });
    await userEvent.click(screen.getByRole('button', { name: 'Share' }));
    await userEvent.click(screen.getByRole('button', { name: 'Stop it' }));
    expect(onStop).toHaveBeenCalledOnce();
  });
});

describe('ShareButton: a whole screen with system audio (05 §13.3)', () => {
  it('shows the warning before anything is shared', async () => {
    const src = fakePick('monitor', true);
    const { onStart, store } = renderButton({ sharing: fakeSharing(src) });
    await shareFromSheet();
    expect(await screen.findByRole('dialog', { name: WARNING })).toBeInTheDocument();
    expect(onStart).not.toHaveBeenCalled();
    expect(store.getState().phase).toBe('confirming');
    expect(src.stream.getTracks().map((t) => t.readyState)).toEqual(['live', 'live']);
  });

  it('"Share without sound" stops the audio track and starts without it', async () => {
    const src = fakePick('monitor', true);
    const [audio] = src.stream.getAudioTracks();
    const { onStart, store, toasts } = renderButton({ sharing: fakeSharing(src) });
    await shareFromSheet();
    await userEvent.click(await screen.findByRole('button', { name: 'Share without sound' }));
    await waitFor(() => {
      expect(onStart).toHaveBeenCalledExactlyOnceWith(src, { preset: 'auto', withAudio: false });
    });
    expect(audio?.readyState).toBe('ended');
    expect(src.preview.getAudioTracks()).toEqual([]);
    expect(src.preview.getVideoTracks()[0]?.readyState).toBe('live');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(store.getState()).toMatchObject({ phase: 'starting', withAudio: false });
    expect(toasts()).toEqual([]);
  });

  it('"Share with sound anyway" starts with the audio track', async () => {
    const src = fakePick('monitor', true);
    const { onStart } = renderButton({ sharing: fakeSharing(src) });
    await shareFromSheet();
    await userEvent.click(await screen.findByRole('button', { name: 'Share with sound anyway' }));
    await waitFor(() => {
      expect(onStart).toHaveBeenCalledExactlyOnceWith(src, { preset: 'auto', withAudio: true });
    });
    expect(src.preview.getAudioTracks()[0]?.readyState).toBe('live');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('"Pick something else" releases the stream and reopens the picker', async () => {
    const screenPick = fakePick('monitor', true);
    const windowPick = fakePick('window', true);
    const sharing = fakeSharing(screenPick, windowPick);
    const { onStart } = renderButton({ sharing });
    await userEvent.click(screen.getByRole('button', { name: 'Share' }));
    await userEvent.click(screen.getByRole('radio', { name: /^Movie/ }));
    await userEvent.click(
      within(screen.getByRole('dialog', { name: 'Share your screen' })).getByRole('button', { name: 'Share' }),
    );
    await userEvent.click(await screen.findByRole('button', { name: 'Pick something else' }));
    expect(sharing.pick).toHaveBeenCalledTimes(2);
    expect(sharing.pick).toHaveBeenLastCalledWith({ preset: 'movie' });
    expect(screenPick.release).toHaveBeenCalledOnce();
    expect(screenPick.stream.active).toBe(false);
    await waitFor(() => {
      expect(onStart).toHaveBeenCalledExactlyOnceWith(windowPick, { preset: 'movie', withAudio: true });
    });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('picking the whole screen with sound again brings the warning back', async () => {
    const first = fakePick('monitor', true);
    const second = fakePick('monitor', true);
    const { onStart, store } = renderButton({ sharing: fakeSharing(first, second) });
    await shareFromSheet();
    await userEvent.click(await screen.findByRole('button', { name: 'Pick something else' }));
    await waitFor(() => {
      expect(first.release).toHaveBeenCalledOnce();
    });
    expect(await screen.findByRole('dialog', { name: WARNING })).toBeInTheDocument();
    expect(store.getState().phase).toBe('confirming');
    expect(second.release).not.toHaveBeenCalled();
    expect(onStart).not.toHaveBeenCalled();
  });

  it('cancelling the new picker ends the flow', async () => {
    const first = fakePick('monitor', true);
    const { onStart, store } = renderButton({ sharing: fakeSharing(first, null) });
    await shareFromSheet();
    await userEvent.click(await screen.findByRole('button', { name: 'Pick something else' }));
    await waitFor(() => {
      expect(store.getState().phase).toBe('idle');
    });
    expect(first.release).toHaveBeenCalledOnce();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(onStart).not.toHaveBeenCalled();
  });

  it('✕ gives the pick up: the stream is released and nothing is shared', async () => {
    const src = fakePick('monitor', true);
    const { onStart, store } = renderButton({ sharing: fakeSharing(src) });
    await shareFromSheet();
    const dialog = await screen.findByRole('dialog', { name: WARNING });
    await userEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
    expect(src.release).toHaveBeenCalledOnce();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(store.getState().phase).toBe('idle');
    expect(onStart).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Share' })).toBeEnabled();
  });

  it('"Get the desktop app" gives the pick up and leaves for /download', async () => {
    const src = fakePick('monitor', true);
    const { onStart, store, router } = renderButton({ sharing: fakeSharing(src) });
    await shareFromSheet();
    await userEvent.click(await screen.findByRole('link', { name: 'Get the desktop app' }));
    expect(router.state.location.pathname).toBe('/download');
    expect(src.release).toHaveBeenCalledOnce();
    expect(store.getState().phase).toBe('idle');
    expect(onStart).not.toHaveBeenCalled();
  });

  it('leaving the page while the warning is open releases the capture', async () => {
    const src = fakePick('monitor', true);
    const { store, unmount } = renderButton({ sharing: fakeSharing(src) });
    await shareFromSheet();
    await screen.findByRole('dialog', { name: WARNING });
    unmount();
    expect(src.release).toHaveBeenCalledOnce();
    expect(store.getState().phase).toBe('idle');
  });

  it('leaving the page while the picker is open releases what it returns later', async () => {
    const picking = deferred<PickedSource | null>();
    const sharing = fakeSharing();
    sharing.pick.mockReturnValueOnce(picking.promise);
    const { store, onStart, unmount } = renderButton({ sharing });
    await shareFromSheet();
    unmount();
    expect(store.getState().phase).toBe('idle');
    const src = fakePick('window', true);
    picking.resolve(src);
    await waitFor(() => {
      expect(src.release).toHaveBeenCalledOnce();
    });
    expect(onStart).not.toHaveBeenCalled();
  });

  it('leaving the page once the share is starting leaves it alone', async () => {
    const src = fakePick('window', true);
    const { store, onStart, unmount } = renderButton({ sharing: fakeSharing(src) });
    onStart.mockReturnValueOnce(new Promise(() => undefined));
    await shareFromSheet();
    await waitFor(() => {
      expect(store.getState().phase).toBe('starting');
    });
    unmount();
    expect(store.getState().phase).toBe('starting');
    expect(src.release).not.toHaveBeenCalled();
  });

  it('with two Share buttons on the page, only the one that was used shows the warning', async () => {
    const src = fakePick('monitor', true);
    renderButton({ sharing: fakeSharing(src), second: true });
    await shareFromSheet('Share your screen');
    await screen.findByRole('dialog', { name: WARNING });
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    expect(screen.getAllByRole('button', { name: 'Share without sound' })).toHaveLength(1);
  });

  it('the browser\'s own "Stop sharing" under the warning closes it: nothing is left to confirm', async () => {
    const src = fakePick('monitor', true);
    const { onStart, store } = renderButton({ sharing: fakeSharing(src) });
    await shareFromSheet();
    await screen.findByRole('dialog', { name: WARNING });
    act(() => {
      src.stream.getVideoTracks()[0]?.end();
    });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(store.getState().phase).toBe('idle');
    expect(src.release).toHaveBeenCalledOnce();
    expect(onStart).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Share' })).toBeEnabled();
  });

  it("a warning that ended that way does not come back on the other button's pick", async () => {
    const first = fakePick('monitor', true);
    const second = fakePick('monitor', true);
    renderButton({ sharing: fakeSharing(first, second), second: true });
    await shareFromSheet();
    await screen.findByRole('dialog', { name: WARNING });
    act(() => {
      first.stream.getVideoTracks()[0]?.end();
    });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await shareFromSheet('Share your screen');
    await screen.findByRole('dialog', { name: WARNING });
    expect(screen.getAllByRole('dialog')).toHaveLength(1);
  });
});
