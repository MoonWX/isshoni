import { act, fireEvent, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { PickedSource } from '../platform/types';
import type { ClientOS } from '../protocol/types.gen';
import { createHarness, tick, type Harness } from '../rooms/testing/harness';
import { stubRest } from '../rooms/testing/page';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderRoute } from '../test/render';
import { ScreenAudioWarning, type ScreenAudioWarningProps } from './ScreenAudioWarning';
import { deferred, fakeSharing, type FakeSharing } from './testing/fakeCapture';

function renderWarning(
  {
    sharing = fakeSharing(),
    ...props
  }: Partial<Omit<ScreenAudioWarningProps, 'sharing'>> & { sharing?: FakeSharing } = {},
  os: ClientOS = 'windows',
) {
  const onContinue = vi.fn<ScreenAudioWarningProps['onContinue']>();
  const onPickAgain = vi.fn<ScreenAudioWarningProps['onPickAgain']>();
  const onCancel = vi.fn();
  const platform = createTestPlatform({
    sharing,
    client: { kind: 'web', version: '0.0.0-test', os, browser: 'chrome' },
  });
  const result = renderRoute({
    services: createTestServices({ platform }),
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

  it('outside Windows: no link, "Desktop app: Windows now, Mac soon" (M2 03 §12.3)', () => {
    const { dialog, onCancel } = renderWarning({}, 'macos');
    expect(within(dialog).queryByRole('link')).not.toBeInTheDocument();
    expect(within(dialog).getByText('Desktop app: Windows now, Mac soon')).toBeInTheDocument();
    expect(onCancel).not.toHaveBeenCalled();
  });
});

describe('ScreenAudioWarning in a room (M2 03 §12.3)', () => {
  let h: Harness;

  beforeEach(() => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
  });

  afterEach(() => {
    h.close();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('"Get the desktop app" opens /download with the room, under "Open in the isshoni app"', async () => {
    h = createHarness({
      server: { welcome: { features: ['agent.relay', 'user.connections'] } },
      platform: {
        serverOrigin: 'https://watch.example.com',
        client: { kind: 'web', version: '0.2.0', os: 'windows', browser: 'chrome' },
      },
    });
    stubRest(h.platform);
    const joined = h.runtime.session.join('lounge');
    await h.connect();
    await joined;
    h.server.send('user.connections', {
      connections: [
        { id: 'c_desk', kind: 'desktop', role: 'viewer', status: 'online', os: 'windows', version: '0.2.0' },
      ],
    });
    await tick();
    const onCancel = vi.fn();
    const { router } = renderRoute({
      services: h.services,
      routes: [
        {
          path: '/',
          element: (
            <ScreenAudioWarning
              open
              sharing={fakeSharing()}
              preset="movie"
              onContinue={vi.fn()}
              onPickAgain={vi.fn()}
              onCancel={onCancel}
            />
          ),
        },
        { path: '/download', element: <h1>Download page</h1> },
      ],
    });
    await act(() => tick());
    const dialog = screen.getByRole('dialog', { name: 'Share all sound on this computer?' });
    const open = within(dialog).getByRole('button', { name: 'Open in the isshoni app' });
    const link = within(dialog).getByRole('link', { name: 'Get the desktop app' });
    expect(link).toHaveAttribute('href', '/download?room=lounge');
    expect(open.compareDocumentPosition(link) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    fireEvent.click(link);
    expect(onCancel).toHaveBeenCalledOnce();
    expect(router.state.location.pathname + router.state.location.search).toBe('/download?room=lounge');
  });
});
