import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { PREFS_KEY } from '../app/prefs';
import { LocalError } from '../lib/errors';
import type { PickedSource } from '../platform/types';
import type { Preset } from '../protocol/types.gen';
import { createTestPlatform } from '../test/platform';
import { createTestServices, renderRoute } from '../test/render';
import { ShareSheet, type ShareSheetProps } from './ShareSheet';
import { deferred, fakePick, fakeSharing, type FakeSharing } from './testing/fakeCapture';

type OnPick = ShareSheetProps['onPick'];

function renderSheet(
  { sharing = fakeSharing(), ...props }: Partial<Omit<ShareSheetProps, 'sharing'>> & { sharing?: FakeSharing } = {},
  browser = 'chrome',
) {
  const platform = createTestPlatform({
    sharing,
    client: { kind: 'web', version: '0.0.0-test', os: 'other', browser },
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
    expect(link).toHaveAttribute('href', '/download');
    await userEvent.click(link);
    expect(router.state.location.pathname).toBe('/download');
  });

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
