import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { PREFS_KEY } from '../app/prefs';
import { LocalError } from '../lib/errors';
import type { PickedSource } from '../platform/types';
import type { ClientOS, OwnConnection, Preset } from '../protocol/types.gen';
import { createHarness, tick, type Harness } from '../rooms/testing/harness';
import { stubRest } from '../rooms/testing/page';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderRoute } from '../test/render';
import { ShareSheet, type ShareSheetProps } from './ShareSheet';
import { deferred, fakePick, fakeSharing, type FakeSharing } from './testing/fakeCapture';

type OnPick = ShareSheetProps['onPick'];

function renderSheet(
  { sharing = fakeSharing(), ...props }: Partial<Omit<ShareSheetProps, 'sharing'>> & { sharing?: FakeSharing } = {},
  browser = 'chrome',
  os: ClientOS = 'windows',
) {
  const platform = createTestPlatform({
    sharing,
    client: { kind: 'web', version: '0.0.0-test', os, browser },
  });
  const onPick = vi.fn<OnPick>();
  const onClose = vi.fn();
  const result = renderRoute({
    services: createTestServices({ platform }),
    routes: [
      {
        path: '/',
        element: <ShareSheet open sharing={sharing} onPick={onPick} onClose={onClose} {...props} />,
      },
      { path: '/download', element: <h1>Download page</h1> },
    ],
  });
  const sheet = screen.getByRole('dialog', { name: 'Share your screen' });
  return { ...result, sharing, onPick, onClose, sheet };
}

describe('ShareSheet (05 §13.1)', () => {
  it('lists the presets, one line each, with Auto chosen at first', () => {
    const { sheet } = renderSheet();
    const radios = within(sheet).getAllByRole('radio');
    expect(radios.map((r) => r.closest('label')?.textContent)).toEqual([
      'AutoA good balance for most things',
      'GameSmooth motion comes first',
      'MovieSmooth motion, and better sound',
      'TextSharp text comes first',
    ]);
    expect(within(sheet).getByRole('group', { name: 'What are you sharing?' })).toBeInTheDocument();
    expect(within(sheet).getByRole('radio', { name: /^Auto/ })).toBeChecked();
  });

  it('recommends a window with its own sound, and offers the desktop app', async () => {
    const { sheet, router } = renderSheet();
    const tip = within(sheet).getByText(/^Recommended: pick a/);
    expect(tip).toHaveTextContent('Recommended: pick a Window and keep Share audio on. Friends hear only that window.');
    expect([...tip.querySelectorAll('strong')].map((b) => b.textContent)).toEqual(['Window', 'Share audio']);
    const link = within(sheet).getByRole('link', { name: 'Get the desktop app (keeps Discord out of your audio)' });
    // Outside a room (no room runtime): the plain page.
    expect(link).toHaveAttribute('href', '/download');
    await userEvent.click(link);
    expect(router.state.location.pathname).toBe('/download');
    expect(within(sheet).queryByText('Desktop app: Windows now, Mac soon')).not.toBeInTheDocument();
  });

  it.each(['macos', 'linux', 'android', 'ios', 'other'] as const)(
    'on %s: no link, but "Desktop app: Windows now, Mac soon" (M2 03 §12.3)',
    (os) => {
      const { sheet } = renderSheet({}, 'chrome', os);
      expect(within(sheet).queryByRole('link')).not.toBeInTheDocument();
      expect(within(sheet).getByText('Desktop app: Windows now, Mac soon')).toBeInTheDocument();
    },
  );

  it('Share opens the picker inside the click, then hands the pick over', () => {
    const picking = deferred<PickedSource | null>();
    const sharing = fakeSharing();
    const order: string[] = [];
    sharing.pick.mockImplementationOnce(() => {
      order.push('pick');
      return picking.promise;
    });
    const { sheet, onPick } = renderSheet({ sharing });
    onPick.mockImplementation(() => {
      order.push('onPick');
    });
    // fireEvent dispatches synchronously: by the time it returns, the click handler has run, and nothing in it
    // could have been awaited (the picker needs the click's transient activation).
    fireEvent.click(within(sheet).getByRole('button', { name: 'Share' }));
    expect(sharing.pick).toHaveBeenCalledExactlyOnceWith({ preset: 'auto' });
    expect(onPick).toHaveBeenCalledExactlyOnceWith(picking.promise, 'auto');
    expect(order).toEqual(['pick', 'onPick']);
  });

  it.each<[RegExp, Preset]>([
    [/^Game/, 'game'],
    [/^Movie/, 'movie'],
    [/^Text/, 'text'],
  ])('a chosen preset (%s) goes into the pick and is remembered on this device', async (name, preset) => {
    const { sheet, sharing, onPick, services } = renderSheet();
    await userEvent.click(within(sheet).getByRole('radio', { name }));
    expect(within(sheet).getByRole('radio', { name })).toBeChecked();
    expect(services.prefs.getState().preset).toBe(preset);
    expect(JSON.parse(services.platform.storage.local.get(PREFS_KEY) ?? '{}')).toMatchObject({ preset });
    await userEvent.click(within(sheet).getByRole('button', { name: 'Share' }));
    expect(sharing.pick).toHaveBeenCalledExactlyOnceWith({ preset });
    expect(onPick.mock.calls[0]?.[1]).toBe(preset);
  });

  it('starts from the preset used last time', () => {
    const platform = createTestPlatform({ sharing: fakeSharing() });
    platform.storage.local.set(PREFS_KEY, JSON.stringify({ preset: 'movie' }));
    renderRoute({
      services: createTestServices({ platform }),
      routes: [{ path: '/', element: <ShareSheet open sharing={fakeSharing()} onPick={vi.fn()} onClose={vi.fn()} /> }],
    });
    expect(screen.getByRole('radio', { name: /^Movie/ })).toBeChecked();
  });

  it('Cancel and ✕ close the sheet without opening the picker', async () => {
    const { sheet, sharing, onClose } = renderSheet();
    await userEvent.click(within(sheet).getByRole('button', { name: 'Cancel' }));
    await userEvent.click(within(sheet).getByRole('button', { name: 'Close' }));
    expect(onClose).toHaveBeenCalledTimes(2);
    expect(sharing.pick).not.toHaveBeenCalled();
  });

  describe('which browsers can share (05 §13.8)', () => {
    it.each(['chrome', 'edge'])('says nothing extra in %s, the tested sharers', (browser) => {
      renderSheet({}, browser);
      expect(screen.queryByText('Sharing works best in Chrome or Edge.')).not.toBeInTheDocument();
    });

    it.each(['firefox', 'safari', 'other'])(
      'still shares in %s, labelled "works best in Chrome or Edge"',
      (browser) => {
        const { sheet } = renderSheet({}, browser);
        expect(within(sheet).getByText('Sharing works best in Chrome or Edge.')).toBeInTheDocument();
        expect(within(sheet).getByRole('button', { name: 'Share' })).toBeEnabled();
      },
    );
  });

  describe('already sharing from another tab or device', () => {
    it('says so, offers "Stop it", and starts no second share', async () => {
      const onStop = vi.fn(() => Promise.resolve());
      const { sheet, sharing } = renderSheet({ elsewhere: { onStop } });
      expect(within(sheet).getByText("You're already sharing from another tab or device")).toBeInTheDocument();
      const share = within(sheet).getByRole('button', { name: 'Share' });
      expect(share).toBeDisabled();
      await userEvent.click(share);
      expect(sharing.pick).not.toHaveBeenCalled();
      await userEvent.click(within(sheet).getByRole('button', { name: 'Stop it' }));
      expect(onStop).toHaveBeenCalledOnce();
    });

    it('shows why the other share could not be stopped', async () => {
      const onStop = vi.fn(() => Promise.reject(new LocalError('offline')));
      const { sheet } = renderSheet({ elsewhere: { onStop } });
      await userEvent.click(within(sheet).getByRole('button', { name: 'Stop it' }));
      expect(await within(sheet).findByRole('alert')).toHaveTextContent("You're offline. Check your connection.");
      await waitFor(() => {
        expect(within(sheet).getByRole('button', { name: 'Stop it' })).not.toHaveAttribute('aria-busy');
      });
    });

    it('lets Share through once the other share is gone', () => {
      const { sheet } = renderSheet({ elsewhere: null });
      expect(within(sheet).queryByRole('button', { name: 'Stop it' })).not.toBeInTheDocument();
      expect(within(sheet).getByRole('button', { name: 'Share' })).toBeEnabled();
    });
  });

  it('does not touch the picked source itself', () => {
    const src = fakePick('window', true);
    const sharing = fakeSharing(src);
    const { sheet } = renderSheet({ sharing });
    fireEvent.click(within(sheet).getByRole('button', { name: 'Share' }));
    expect(src.release).not.toHaveBeenCalled();
    expect(src.stream.active).toBe(true);
  });
});

describe('ShareSheet in a room: the desktop app’s entries (M2 03 §12.3)', () => {
  let h: Harness;
  const desktopConn: OwnConnection = {
    id: 'c_desk',
    kind: 'desktop',
    role: 'viewer',
    status: 'online',
    os: 'windows',
    version: '0.2.0',
  };

  beforeEach(() => {
    vi.useFakeTimers();
    vi.spyOn(Math, 'random').mockReturnValue(0.5);
  });

  afterEach(() => {
    h.close();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  /** The sheet over a room runtime in `roomId`, in a Chrome on `os` of https://watch.example.com. */
  async function inRoom(roomId: string, os: ClientOS = 'windows', desktop = false): Promise<HTMLElement> {
    h = createHarness({
      server: { welcome: { features: ['agent.relay', 'user.connections'] } },
      platform: {
        serverOrigin: 'https://watch.example.com',
        client: { kind: 'web', version: '0.2.0', os, browser: 'chrome' },
      },
    });
    stubRest(h.platform);
    const joined = h.runtime.session.join(roomId);
    await h.connect();
    await joined;
    if (desktop) h.server.send('user.connections', { connections: [desktopConn] });
    await tick();
    renderRoute({
      services: h.services,
      routes: [
        {
          path: '/',
          element: <ShareSheet open sharing={fakeSharing()} onPick={vi.fn<OnPick>()} onClose={vi.fn()} />,
        },
        { path: '/download', element: <h1>Download page</h1> },
      ],
    });
    await act(() => tick());
    return screen.getByRole('dialog', { name: 'Share your screen' });
  }

  it('"Get the desktop app" opens /download with the room the user is in', async () => {
    const sheet = await inRoom('lounge');
    const link = within(sheet).getByRole('link', { name: 'Get the desktop app (keeps Discord out of your audio)' });
    expect(link).toHaveAttribute('href', '/download?room=lounge');
    // Without a desktop app in sight, no "Open in the isshoni app".
    expect(within(sheet).queryByRole('button', { name: 'Open in the isshoni app' })).not.toBeInTheDocument();
  });

  it('"Open in the isshoni app" comes above it when the user probably has the app', async () => {
    const sheet = await inRoom('lounge', 'windows', true);
    const open = within(sheet).getByRole('button', { name: 'Open in the isshoni app' });
    const link = within(sheet).getByRole('link', { name: 'Get the desktop app (keeps Discord out of your audio)' });
    expect(open.compareDocumentPosition(link) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('on a Mac: "Open in the isshoni app" when the app is likely, and the Windows-only text instead of the link', async () => {
    const sheet = await inRoom('lounge', 'macos', true);
    expect(within(sheet).getByRole('button', { name: 'Open in the isshoni app' })).toBeInTheDocument();
    expect(within(sheet).queryByRole('link')).not.toBeInTheDocument();
    expect(within(sheet).getByText('Desktop app: Windows now, Mac soon')).toBeInTheDocument();
  });
});
