import { fireEvent, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { PickedSource } from '../platform/types';
import { renderRoute } from '../test/render';
import { ScreenAudioWarning, type ScreenAudioWarningProps } from './ScreenAudioWarning';
import { deferred, fakeSharing, type FakeSharing } from './testing/fakeCapture';

function renderWarning({
  sharing = fakeSharing(),
  ...props
}: Partial<Omit<ScreenAudioWarningProps, 'sharing'>> & { sharing?: FakeSharing } = {}) {
  const onContinue = vi.fn<ScreenAudioWarningProps['onContinue']>();
  const onPickAgain = vi.fn<ScreenAudioWarningProps['onPickAgain']>();
  const onCancel = vi.fn();
  const result = renderRoute({
    routes: [
      {
        path: '/',
        element: (
          <ScreenAudioWarning
            open
            sharing={sharing}
            preset="movie"
            onContinue={onContinue}
            onPickAgain={onPickAgain}
            onCancel={onCancel}
            {...props}
          />
        ),
      },
      { path: '/download', element: <h1>Download page</h1> },
    ],
  });
  const dialog = screen.getByRole('dialog', { name: 'Share all sound on this computer?' });
  return { ...result, sharing, onContinue, onPickAgain, onCancel, dialog };
}

describe('ScreenAudioWarning (05 §13.3)', () => {
  it('says what friends would hear, and why a browser cannot help it', () => {
    const { dialog } = renderWarning();
    expect(dialog).toHaveAttribute('open');
    expect(
      within(dialog).getByText(
        "Friends will hear everything on this computer, including your voice app (Discord, TeamSpeak…), so they'll hear themselves.",
      ),
    ).toBeInTheDocument();
    const why = within(dialog).getByText(/can't leave apps out/);
    expect(why).toHaveTextContent("Discord's own screen share on the web has the same problem");
    expect(why).toHaveTextContent('The isshoni desktop app keeps voice apps out of the sound.');
  });

  it('offers exactly the three ways on, most careful one last (the primary button)', () => {
    const { dialog } = renderWarning();
    const footer = within(dialog).getByRole('button', { name: 'Share without sound' }).closest('footer');
    expect(footer).not.toBeNull();
    expect(
      within(footer as HTMLElement)
        .getAllByRole('button')
        .map((b) => b.textContent),
    ).toEqual(['Pick something else', 'Share with sound anyway', 'Share without sound']);
  });

  it('"Share without sound" continues without the audio', async () => {
    const { dialog, onContinue, onPickAgain, onCancel, sharing } = renderWarning();
    await userEvent.click(within(dialog).getByRole('button', { name: 'Share without sound' }));
    expect(onContinue).toHaveBeenCalledExactlyOnceWith(false);
    expect(onPickAgain).not.toHaveBeenCalled();
    expect(onCancel).not.toHaveBeenCalled();
    expect(sharing.pick).not.toHaveBeenCalled();
  });

  it('"Share with sound anyway" continues with it', async () => {
    const { dialog, onContinue, onCancel, sharing } = renderWarning();
    await userEvent.click(within(dialog).getByRole('button', { name: 'Share with sound anyway' }));
    expect(onContinue).toHaveBeenCalledExactlyOnceWith(true);
    expect(onCancel).not.toHaveBeenCalled();
    expect(sharing.pick).not.toHaveBeenCalled();
  });

  it('"Pick something else" reopens the picker from this click, with the same preset', () => {
    const picking = deferred<PickedSource | null>();
    const sharing = fakeSharing();
    const order: string[] = [];
    sharing.pick.mockImplementationOnce(() => {
      order.push('pick');
      return picking.promise;
    });
    const { dialog, onPickAgain, onContinue, onCancel } = renderWarning({ sharing });
    onPickAgain.mockImplementation(() => {
      order.push('onPickAgain');
    });
    // Synchronous dispatch: the picker opened inside the click handler (transient activation).
    fireEvent.click(within(dialog).getByRole('button', { name: 'Pick something else' }));
    expect(sharing.pick).toHaveBeenCalledExactlyOnceWith({ preset: 'movie' });
    expect(onPickAgain).toHaveBeenCalledExactlyOnceWith(picking.promise);
    expect(order).toEqual(['pick', 'onPickAgain']);
    expect(onContinue).not.toHaveBeenCalled();
    expect(onCancel).not.toHaveBeenCalled();
  });

  it('✕ and Esc give the pick up', async () => {
    const { dialog, onCancel, onContinue } = renderWarning();
    await userEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
    expect(onCancel).toHaveBeenCalledTimes(1);
    fireEvent(dialog, new Event('cancel', { cancelable: true }));
    expect(onCancel).toHaveBeenCalledTimes(2);
    expect(onContinue).not.toHaveBeenCalled();
  });

  it('"Get the desktop app" gives the pick up and goes to /download', async () => {
    const { dialog, onCancel, router } = renderWarning();
    const link = within(dialog).getByRole('link', { name: 'Get the desktop app' });
    expect(link).toHaveAttribute('href', '/download');
    await userEvent.click(link);
    expect(onCancel).toHaveBeenCalledOnce();
    expect(router.state.location.pathname).toBe('/download');
  });
});
