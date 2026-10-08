// The Share button with the whole pick flow behind it: sheet → picker → (warning) → start (05 §13.1, §13.3, §13.8).
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi, type Mock } from 'vitest';
import { useStore } from 'zustand';
import { createStore, type StoreApi } from 'zustand/vanilla';

import { LocalError, NotImplementedError } from '../lib/errors';
import type { PickedSource } from '../platform/types';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderRoute } from '../test/render';
import { NOTE_TOAST_MS, ShareButton, ShareUnavailableNote, type ShareButtonProps } from './ShareButton';
import { resetLevelAudio } from './levelAudio';
import { createShareStore, type StartShare } from './shareStore';
import { deferred, FakeAudioContext, fakePick, fakeSharing, type FakeSharing } from './testing/fakeCapture';
import { shareParams } from './testing/publish';

const WARNING = 'Share all sound on this computer?';

interface Setup extends Partial<ShareButtonProps> {
  sharing?: FakeSharing | null;
  /** A second Share button on the same page (the room's empty state). */
  second?: boolean;
}

/** Which of the page's Share buttons are mounted: "Share" (the header) and "Share your screen" (the empty state). */
interface Mounted {
  header: boolean;
  emptyState: boolean;
}

/** The room page as far as sharing goes: the header's button first, then the empty state's. */
function Buttons({ mounted, ...props }: ShareButtonProps & { mounted: StoreApi<Mounted> }) {
  const { header, emptyState } = useStore(mounted);
  return (
    <>
      {header && <ShareButton {...props} />}
      {emptyState && <ShareButton label="Share your screen" {...props} />}
    </>
  );
}

function renderButton({ sharing = fakeSharing(), second = false, ...props }: Setup = {}) {
  const store = props.store ?? createShareStore();
  const onStart: Mock<StartShare> = vi.fn<StartShare>(() => Promise.resolve());
  const services = createTestServices({ platform: createTestPlatform({ sharing }) });
  const mounted = createStore<Mounted>(() => ({ header: true, emptyState: second }));
  const result = renderRoute({
    services,
    routes: [
      { path: '/', element: <Buttons mounted={mounted} onStart={onStart} store={store} {...props} /> },
      { path: '/download', element: <h1>Download page</h1> },
    ],
  });
  const toasts = () => services.ui.getState().toasts.map((t) => ({ kind: t.kind, message: t.message }));
  /** Takes one of the buttons off the page, as the room page does with the empty state once somebody shares. */
  const remove = (which: keyof Mounted): void => {
    act(() => {
      mounted.setState({ [which]: false });
    });
  };
  return { ...result, store, onStart, sharing, services, toasts, remove };
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
    // Any error will do: this one has no text of its own.
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

  it.each(['Share', 'Share your screen'])(
    'with two Share buttons on the page, the warning shows once (picked from "%s")',
    async (used) => {
      const src = fakePick('monitor', true);
      const { onStart } = renderButton({ sharing: fakeSharing(src), second: true });
      await shareFromSheet(used);
      await screen.findByRole('dialog', { name: WARNING });
      expect(screen.getAllByRole('dialog')).toHaveLength(1);
      await userEvent.click(screen.getByRole('button', { name: 'Share without sound' }));
      await waitFor(() => {
        expect(onStart).toHaveBeenCalledExactlyOnceWith(src, { preset: 'auto', withAudio: false });
      });
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    },
  );

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

// The room page shows two Share buttons, and the one in the empty state disappears the moment a friend's share
// arrives (05 §11.2). Two friends clicking "Share your screen" in an empty room at about the same time is a normal
// case: the pick of the one who was a little later must not be dropped.
describe('ShareButton: the button that was used goes away mid-flow', () => {
  it('under the open picker: a window pick still starts', async () => {
    const picking = deferred<PickedSource | null>();
    const sharing = fakeSharing();
    sharing.pick.mockReturnValueOnce(picking.promise);
    const { store, onStart, remove } = renderButton({ sharing, second: true });
    await shareFromSheet('Share your screen');
    remove('emptyState');
    expect(screen.queryByRole('button', { name: 'Share your screen' })).not.toBeInTheDocument();
    expect(store.getState().phase).toBe('picking');
    expect(screen.getByRole('button', { name: 'Share' })).toBeDisabled();

    const src = fakePick('window', true);
    picking.resolve(src);
    await waitFor(() => {
      expect(onStart).toHaveBeenCalledExactlyOnceWith(src, { preset: 'auto', withAudio: true });
    });
    expect(src.release).not.toHaveBeenCalled();
    expect(store.getState().phase).toBe('starting');
  });

  it('under the open picker: the "no sound" note is still shown', async () => {
    const picking = deferred<PickedSource | null>();
    const sharing = fakeSharing();
    sharing.pick.mockReturnValueOnce(picking.promise);
    const { onStart, toasts, remove } = renderButton({ sharing, second: true });
    await shareFromSheet('Share your screen');
    remove('emptyState');
    const src = fakePick('monitor', false);
    picking.resolve(src);
    await waitFor(() => {
      expect(toasts()).toEqual([{ kind: 'info', message: 'No sound is shared' }]);
    });
    expect(onStart).toHaveBeenCalledExactlyOnceWith(src, { preset: 'auto', withAudio: false });
  });

  it('under the open picker: a capture that fails is still reported, and Share is usable again', async () => {
    const picking = deferred<PickedSource | null>();
    const sharing = fakeSharing();
    sharing.pick.mockReturnValueOnce(picking.promise);
    const { store, toasts, remove } = renderButton({ sharing, second: true });
    await shareFromSheet('Share your screen');
    remove('emptyState');
    picking.reject(new LocalError('capture_failed'));
    await waitFor(() => {
      expect(toasts()).toEqual([{ kind: 'error', message: "Couldn't capture that. Another app may be blocking it." }]);
    });
    expect(store.getState().phase).toBe('idle');
    expect(screen.getByRole('button', { name: 'Share' })).toBeEnabled();
  });

  it('under the open picker: a whole screen with sound brings the warning up on the button that is left', async () => {
    const picking = deferred<PickedSource | null>();
    const sharing = fakeSharing();
    sharing.pick.mockReturnValueOnce(picking.promise);
    const { store, onStart, remove } = renderButton({ sharing, second: true });
    await shareFromSheet('Share your screen');
    remove('emptyState');

    const src = fakePick('monitor', true);
    picking.resolve(src);
    await screen.findByRole('dialog', { name: WARNING });
    expect(onStart).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole('button', { name: 'Share without sound' }));
    await waitFor(() => {
      expect(onStart).toHaveBeenCalledExactlyOnceWith(src, { preset: 'auto', withAudio: false });
    });
    expect(src.preview.getAudioTracks()).toEqual([]);
    expect(store.getState()).toMatchObject({ phase: 'starting', withAudio: false });
    expect(src.release).not.toHaveBeenCalled();
  });

  it('under the warning: it stays up, and "Share without sound" starts without the audio track', async () => {
    const src = fakePick('monitor', true);
    const { store, onStart, remove } = renderButton({ sharing: fakeSharing(src), second: true });
    await shareFromSheet('Share your screen');
    await screen.findByRole('dialog', { name: WARNING });
    remove('emptyState');
    expect(screen.getByRole('dialog', { name: WARNING })).toBeInTheDocument();
    expect(store.getState().phase).toBe('confirming');
    expect(src.release).not.toHaveBeenCalled();

    await userEvent.click(screen.getByRole('button', { name: 'Share without sound' }));
    await waitFor(() => {
      expect(onStart).toHaveBeenCalledExactlyOnceWith(src, { preset: 'auto', withAudio: false });
    });
    expect(src.preview.getAudioTracks()).toEqual([]);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('under the warning: "Pick something else" works from the button that is left', async () => {
    const screenPick = fakePick('monitor', true);
    const windowPick = fakePick('window', true);
    const { onStart, remove } = renderButton({ sharing: fakeSharing(screenPick, windowPick), second: true });
    await shareFromSheet('Share your screen');
    await screen.findByRole('dialog', { name: WARNING });
    remove('emptyState');
    await userEvent.click(screen.getByRole('button', { name: 'Pick something else' }));
    expect(screenPick.release).toHaveBeenCalledOnce();
    await waitFor(() => {
      expect(onStart).toHaveBeenCalledExactlyOnceWith(windowPick, { preset: 'auto', withAudio: true });
    });
  });

  it('the button that shows the warning goes away: the other one takes the warning over', async () => {
    const src = fakePick('monitor', true);
    const { store, onStart, remove } = renderButton({ sharing: fakeSharing(src), second: true });
    await shareFromSheet();
    await screen.findByRole('dialog', { name: WARNING });
    remove('header');
    expect(screen.getAllByRole('dialog', { name: WARNING })).toHaveLength(1);
    expect(store.getState().phase).toBe('confirming');
    expect(src.release).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole('button', { name: 'Share with sound anyway' }));
    await waitFor(() => {
      expect(onStart).toHaveBeenCalledExactlyOnceWith(src, { preset: 'auto', withAudio: true });
    });
  });

  it('the last button going away under the warning releases the capture', async () => {
    const src = fakePick('monitor', true);
    const { store, onStart, remove } = renderButton({ sharing: fakeSharing(src), second: true });
    await shareFromSheet('Share your screen');
    await screen.findByRole('dialog', { name: WARNING });
    remove('emptyState');
    expect(src.release).not.toHaveBeenCalled();
    remove('header');
    expect(src.release).toHaveBeenCalledOnce();
    expect(store.getState().phase).toBe('idle');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(onStart).not.toHaveBeenCalled();
  });

  it('the last button going away under the open picker releases what it returns later', async () => {
    const picking = deferred<PickedSource | null>();
    const sharing = fakeSharing();
    sharing.pick.mockReturnValueOnce(picking.promise);
    const { store, onStart, remove } = renderButton({ sharing, second: true });
    await shareFromSheet('Share your screen');
    remove('header');
    expect(store.getState().phase).toBe('picking');
    remove('emptyState');
    expect(store.getState().phase).toBe('idle');
    const src = fakePick('window', true);
    picking.resolve(src);
    await waitFor(() => {
      expect(src.release).toHaveBeenCalledOnce();
    });
    expect(onStart).not.toHaveBeenCalled();
  });

  it('leaving the page with both buttons on it releases the capture', async () => {
    const src = fakePick('monitor', true);
    const { store, unmount } = renderButton({ sharing: fakeSharing(src), second: true });
    await shareFromSheet('Share your screen');
    await screen.findByRole('dialog', { name: WARNING });
    unmount();
    expect(src.release).toHaveBeenCalledOnce();
    expect(store.getState()).toMatchObject({ phase: 'idle', hostId: null });
  });
});

// The sheet has the focus and is unmounted while open; its Share click also disables the button that opened it.
// Without help the focus would be left on <body> (05 §16.6).
describe('ShareButton: focus comes back to the button', () => {
  const share = (name = 'Share') => screen.getByRole('button', { name });

  it('when the sheet is cancelled', async () => {
    renderButton();
    await userEvent.click(share());
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(share()).toHaveFocus();
  });

  it('when the picker is cancelled', async () => {
    const picking = deferred<PickedSource | null>();
    const sharing = fakeSharing();
    sharing.pick.mockReturnValueOnce(picking.promise);
    renderButton({ sharing });
    await shareFromSheet();
    // The sheet is gone and the button is disabled: nothing has the focus while the picker is open.
    expect(share()).toBeDisabled();
    expect(document.body).toHaveFocus();
    picking.resolve(null);
    await waitFor(() => {
      expect(share()).toHaveFocus();
    });
  });

  it('when the picker is cancelled at once', async () => {
    renderButton();
    await shareFromSheet();
    await waitFor(() => {
      expect(share()).toHaveFocus();
    });
  });

  it('when the capture fails', async () => {
    renderButton({ sharing: fakeSharing(new LocalError('capture_failed')) });
    await shareFromSheet();
    await waitFor(() => {
      expect(share()).toHaveFocus();
    });
  });

  it('when the start fails', async () => {
    const { onStart, store } = renderButton({ sharing: fakeSharing(fakePick('window', true)) });
    onStart.mockRejectedValueOnce(new NotImplementedError('sharing.start', 'S46'));
    await shareFromSheet();
    await waitFor(() => {
      expect(share()).toHaveFocus();
    });
    expect(store.getState().phase).toBe('failed');
  });

  it('when the warning is closed with ✕', async () => {
    renderButton({ sharing: fakeSharing(fakePick('monitor', true)) });
    await shareFromSheet();
    const dialog = await screen.findByRole('dialog', { name: WARNING });
    await userEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
    expect(share()).toHaveFocus();
  });

  it('when the capture ends under the warning', async () => {
    const src = fakePick('monitor', true);
    renderButton({ sharing: fakeSharing(src) });
    await shareFromSheet();
    await screen.findByRole('dialog', { name: WARNING });
    act(() => {
      src.stream.getVideoTracks()[0]?.end();
    });
    expect(share()).toHaveFocus();
  });

  it('when the picker reopened by "Pick something else" is cancelled', async () => {
    renderButton({ sharing: fakeSharing(fakePick('monitor', true), null) });
    await shareFromSheet();
    await userEvent.click(await screen.findByRole('button', { name: 'Pick something else' }));
    await waitFor(() => {
      expect(share()).toHaveFocus();
    });
  });

  it('to the one that was used when the page has two, whichever shows the warning', async () => {
    renderButton({ sharing: fakeSharing(null, fakePick('monitor', true)), second: true });
    await shareFromSheet('Share your screen');
    await waitFor(() => {
      expect(share('Share your screen')).toHaveFocus();
    });

    await shareFromSheet('Share your screen');
    const dialog = await screen.findByRole('dialog', { name: WARNING });
    await userEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
    expect(share('Share your screen')).toHaveFocus();
  });

  it('not when the focus has gone somewhere else meanwhile', async () => {
    const picking = deferred<PickedSource | null>();
    const sharing = fakeSharing();
    sharing.pick.mockReturnValueOnce(picking.promise);
    const { store } = renderButton({ sharing });
    await shareFromSheet();
    const elsewhere = document.createElement('button');
    document.body.append(elsewhere);
    try {
      elsewhere.focus();
      picking.resolve(null);
      await waitFor(() => {
        expect(store.getState().phase).toBe('idle');
      });
      expect(share()).toBeEnabled();
      expect(elsewhere).toHaveFocus();
    } finally {
      elsewhere.remove();
    }
  });

  it('not when the share started: its end, much later, is not this flow', async () => {
    const { store, onStart } = renderButton({ sharing: fakeSharing(fakePick('window', true)) });
    await shareFromSheet();
    await waitFor(() => {
      expect(onStart).toHaveBeenCalledOnce();
    });
    // What the publisher does: live, then stopped.
    act(() => {
      store.getState().advance('live');
    });
    act(() => {
      store.getState().finish();
    });
    expect(share()).toBeEnabled();
    expect(document.body).toHaveFocus();
  });
});

describe('ShareButton: the share state and the rest of the app', () => {
  it('links the share state to uiStore when it mounts, and the link outlives the button (05 §16.2)', () => {
    const { store, services, remove } = renderButton();
    expect(services.ui.getState().sharing).toBe(false);
    act(() => {
      store.getState().publishing({
        picked: { kind: 'window', audioScope: 'window', warning: null },
        preset: 'auto',
        withAudio: true,
        params: shareParams('s_1'),
      });
    });
    expect(services.ui.getState().sharing).toBe(true);
    // The room page is left (/account): the share goes on, and so does the flag.
    remove('header');
    expect(services.ui.getState().sharing).toBe(true);
    act(() => {
      store.getState().finish();
    });
    expect(services.ui.getState().sharing).toBe(false);
  });

  it('where the platform can’t share there is nothing to link', () => {
    const { store, services } = renderButton({ sharing: null });
    act(() => {
      store.setState({ phase: 'live' });
    });
    expect(services.ui.getState().sharing).toBe(false);
  });

  it('opens the level meter’s AudioContext inside the Share click (05 §13.7)', async () => {
    FakeAudioContext.instances = [];
    vi.stubGlobal('AudioContext', FakeAudioContext);
    try {
      renderButton({ sharing: fakeSharing(fakePick('window', true)) });
      await userEvent.click(screen.getByRole('button', { name: 'Share' }));
      expect(FakeAudioContext.instances).toHaveLength(0);
      await userEvent.click(
        within(screen.getByRole('dialog', { name: 'Share your screen' })).getByRole('button', { name: 'Share' }),
      );
      expect(FakeAudioContext.instances).toHaveLength(1);
      const [ctx] = FakeAudioContext.instances;
      // resume() inside the click is what unlocks it. Nothing is metered yet, so it doesn't keep running: the
      // audio thread is for the level meter, which may never be opened.
      expect(ctx?.resume).toHaveBeenCalledOnce();
      await waitFor(() => {
        expect(ctx?.suspend).toHaveBeenCalledOnce();
      });
      expect(ctx?.state).toBe('suspended');
    } finally {
      resetLevelAudio();
      vi.unstubAllGlobals();
    }
  });
});

describe('ShareButton on a page with a SharePanel: the panel reports what it shows', () => {
  it('a failed start is the panel’s to show: no toast', async () => {
    const store = createShareStore();
    const detach = store.getState().attachPanel();
    const { onStart, toasts } = renderButton({ store, sharing: fakeSharing(fakePick('window', true)) });
    const failure = new LocalError('webrtc_failed');
    onStart.mockRejectedValueOnce(failure);
    await shareFromSheet();
    await waitFor(() => {
      expect(store.getState()).toMatchObject({ phase: 'failed', error: failure });
    });
    expect(toasts()).toEqual([]);
    detach();
  });

  it('a failed capture is still a toast: the panel shows nothing for it', async () => {
    const store = createShareStore();
    store.getState().attachPanel();
    const { toasts } = renderButton({ store, sharing: fakeSharing(new LocalError('capture_failed')) });
    await shareFromSheet();
    await waitFor(() => {
      expect(toasts()).toEqual([{ kind: 'error', message: "Couldn't capture that. Another app may be blocking it." }]);
    });
    expect(store.getState().phase).toBe('idle');
  });

  it('the "no sound is shared" note is the panel’s too: no toast, but it is still said once for screen readers', async () => {
    const store = createShareStore();
    store.getState().attachPanel();
    const { onStart, toasts, services } = renderButton({ store, sharing: fakeSharing(fakePick('monitor', false)) });
    await shareFromSheet();
    await waitFor(() => {
      expect(onStart).toHaveBeenCalledOnce();
    });
    // The panel shows the note in its bar (SharePanel.test.tsx), which is no live region.
    await waitFor(() => {
      expect(services.ui.getState().announcements.polite?.text).toBe('No sound is shared');
    });
    expect(toasts()).toEqual([]);
  });

  it('a share with sound has nothing to say there either', async () => {
    const store = createShareStore();
    store.getState().attachPanel();
    const { onStart, toasts, services } = renderButton({ store, sharing: fakeSharing(fakePick('window', true)) });
    await shareFromSheet();
    await waitFor(() => {
      expect(onStart).toHaveBeenCalledOnce();
    });
    await act(async () => {
      await Promise.resolve();
    });
    expect(toasts()).toEqual([]);
    expect(services.ui.getState().announcements.polite).toBeNull();
  });
});
