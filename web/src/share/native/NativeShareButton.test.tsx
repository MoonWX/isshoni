// The desktop's Share button with its flow (docs/m2/03 §10.2, §10.5, §22.3 "Share machine" and "Engine errors"): the
// desktop ShareSheet, the Windows picker (Share.Pick on a fake bridge), the one-time sheets in order (first-share,
// then dnd) with a cancel at each releasing the pick, and the failures: pick again, self window, permission denied.
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi, type Mock } from 'vitest';

import { BridgeError } from '../../platform/desktop/errors';
import { createTestServices, renderRoute } from '../../test/render';
import type { StartShare } from '../shareStore';
import { NativeShareButton } from './NativeShareButton';
import { nativeSetup, PICKED, type NativeSetup } from './testing';

interface Rendered extends NativeSetup {
  onStart: Mock<StartShare>;
  toasts: () => string[];
}

async function renderNative(
  opts: { confirm?: ('first-share' | 'dnd')[]; onStart?: StartShare; elsewhere?: () => Promise<unknown> } = {},
): Promise<Rendered> {
  const setup = await nativeSetup({ picked: { ...PICKED, confirm: opts.confirm ?? [] } });
  const onStart = vi.fn<StartShare>(opts.onStart ?? (() => Promise.resolve()));
  const services = createTestServices({ platform: setup.platform });
  const elsewhere = opts.elsewhere ? { onStop: opts.elsewhere } : null;
  renderRoute({
    services,
    routes: [
      {
        path: '/',
        element: <NativeShareButton onStart={onStart} store={setup.store} elsewhere={elsewhere} />,
      },
    ],
  });
  const toasts = () => services.ui.getState().toasts.map((t) => t.message);
  return { ...setup, onStart, toasts };
}

const sheet = () => screen.getByRole('dialog', { name: 'Share your screen' });

async function openSheetAndShare(): Promise<void> {
  await userEvent.click(screen.getByRole('button', { name: 'Share' }));
  await userEvent.click(within(sheet()).getByRole('button', { name: 'Share' }));
}

describe('the desktop ShareSheet (docs/m2/03 §10.2)', () => {
  it('has the Optimize for picker and Share sound (on); Share opens the Windows picker with them', async () => {
    const r = await renderNative();
    await userEvent.click(screen.getByRole('button', { name: 'Share' }));
    const s = sheet();
    expect(within(s).getByRole('radio', { name: /^Auto/ })).toBeChecked();
    for (const name of [/^Game/, /^Movie/, /^Text/]) expect(within(s).getByRole('radio', { name })).toBeInTheDocument();
    expect(within(s).getByRole('checkbox', { name: 'Share sound' })).toBeChecked();
    await userEvent.click(within(s).getByRole('radio', { name: /^Game/ }));
    await userEvent.click(within(s).getByRole('button', { name: 'Share' }));
    await waitFor(() => {
      expect(r.onStart).toHaveBeenCalledTimes(1);
    });
    const [src, opts] = r.onStart.mock.calls[0] ?? [];
    expect(src).toMatchObject({ kind: 'window', audioScope: 'system', preview: null, label: 'Minecraft' });
    expect(opts).toEqual({ preset: 'game', withAudio: true });
    expect(r.fake.callsOf('share.pick')).toHaveLength(1);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('without Share sound the share starts without it', async () => {
    const r = await renderNative();
    await userEvent.click(screen.getByRole('button', { name: 'Share' }));
    await userEvent.click(within(sheet()).getByRole('checkbox', { name: 'Share sound' }));
    await userEvent.click(within(sheet()).getByRole('button', { name: 'Share' }));
    await waitFor(() => {
      expect(r.onStart).toHaveBeenCalledWith(expect.objectContaining({ audioScope: 'none' }), {
        preset: 'auto',
        withAudio: false,
      });
    });
  });

  it('says "Choose what to share in the Windows picker…" while it is open; a closed picker comes back to the sheet', async () => {
    const r = await renderNative();
    let close: (v: null) => void = () => undefined;
    r.fake.answers['share.pick'] = () =>
      new Promise<null>((resolve) => {
        close = resolve;
      });
    await openSheetAndShare();
    expect(await within(sheet()).findByText('Choose what to share in the Windows picker…')).toBeInTheDocument();
    close(null);
    await waitFor(() => {
      expect(within(sheet()).queryByText('Choose what to share in the Windows picker…')).not.toBeInTheDocument();
    });
    expect(within(sheet()).getByRole('button', { name: 'Share' })).toBeEnabled();
    expect(r.onStart).not.toHaveBeenCalled();
  });

  it('"You’re already sharing from another tab or device": Stop it, and no Share meanwhile', async () => {
    const onStop = vi.fn(() => Promise.resolve());
    await renderNative({ elsewhere: onStop });
    await userEvent.click(screen.getByRole('button', { name: 'Share' }));
    expect(within(sheet()).getByText("You're already sharing from another tab or device")).toBeInTheDocument();
    expect(within(sheet()).getByRole('button', { name: 'Share' })).toBeDisabled();
    await userEvent.click(within(sheet()).getByRole('button', { name: 'Stop it' }));
    expect(onStop).toHaveBeenCalledTimes(1);
  });
});

describe('the one-time sheets, in order (docs/m2/03 §10.2)', () => {
  it('first-share ("Friends will see: <label>"), then the DND tip, then the share', async () => {
    const r = await renderNative({ confirm: ['first-share', 'dnd'] });
    await openSheetAndShare();
    const first = await screen.findByRole('dialog', { name: 'Ready to go live?' });
    expect(within(first).getByText('Friends will see: Minecraft')).toBeInTheDocument();
    await userEvent.click(within(first).getByRole('radio', { name: /^Text/ }));
    await userEvent.click(within(first).getByRole('button', { name: 'Go live' }));

    const dnd = await screen.findByRole('dialog', { name: 'Sharing your whole screen?' });
    expect(r.onStart).not.toHaveBeenCalled();
    await userEvent.click(within(dnd).getByRole('button', { name: 'Open Do not disturb settings' }));
    expect(r.fake.callsOf('app.openOSSettings')).toEqual([['dnd']]);
    expect(screen.getByRole('dialog', { name: 'Sharing your whole screen?' })).toBeInTheDocument();
    await userEvent.click(within(dnd).getByRole('button', { name: 'Share now' }));
    await waitFor(() => {
      expect(r.onStart).toHaveBeenCalledWith(expect.objectContaining({ label: 'Minecraft' }), {
        preset: 'text',
        withAudio: true,
      });
    });
    expect(r.fake.callsOf('share.release')).toEqual([]);
  });

  it.each([
    ['the first-share sheet', 'Ready to go live?'],
    ['the DND tip', 'Sharing your whole screen?'],
  ])('a cancel at %s releases the pick', async (_what, title) => {
    const r = await renderNative({ confirm: ['first-share', 'dnd'] });
    await openSheetAndShare();
    const first = await screen.findByRole('dialog', { name: 'Ready to go live?' });
    if (title !== 'Ready to go live?') {
      await userEvent.click(within(first).getByRole('button', { name: 'Go live' }));
    }
    const dialog = await screen.findByRole('dialog', { name: title });
    // The ✕ (Esc closes the same way in a browser; jsdom has no <dialog> cancel).
    await userEvent.click(within(dialog).getByRole('button', { name: 'Close' }));
    await waitFor(() => {
      expect(r.fake.callsOf('share.release')).toEqual([[PICKED.pickId]]);
    });
    expect(r.onStart).not.toHaveBeenCalled();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });
});

describe('failures (docs/m2/03 §10.5)', () => {
  it('self_window: the picker opens again with "Pick something other than isshoni"', async () => {
    const r = await renderNative();
    const pick = vi
      .fn()
      .mockRejectedValueOnce(new BridgeError('engine.invalid', { reason: 'self_window' }))
      .mockResolvedValue(PICKED);
    r.fake.answers['share.pick'] = pick;
    await openSheetAndShare();
    await waitFor(() => {
      expect(r.onStart).toHaveBeenCalledTimes(1);
    });
    expect(pick).toHaveBeenCalledTimes(2);
  });

  it('self_window shows its line above the sheet while the picker is open again', async () => {
    const r = await renderNative();
    r.fake.answers['share.pick'] = vi
      .fn()
      .mockRejectedValueOnce(new BridgeError('engine.invalid', { reason: 'self_window' }))
      .mockReturnValue(new Promise(() => undefined));
    await openSheetAndShare();
    expect(await within(sheet()).findByText('Pick something other than isshoni.')).toBeInTheDocument();
    expect(within(sheet()).getByText('Choose what to share in the Windows picker…')).toBeInTheDocument();
  });

  it('pick_expired from Share.Start: the picker opens again without a word', async () => {
    let calls = 0;
    const r = await renderNative({
      onStart: () =>
        ++calls === 1 ? Promise.reject(new BridgeError('pick_expired', { reason: 'pick_expired' })) : Promise.resolve(),
    });
    await openSheetAndShare();
    await waitFor(() => {
      expect(r.onStart).toHaveBeenCalledTimes(2);
    });
    expect(r.fake.callsOf('share.pick')).toHaveLength(2);
    expect(r.toasts()).toEqual([]);
    expect(r.store.getState().phase).toBe('starting');
  });

  it('permission_denied: the sheet says it, with Windows settings and Try again', async () => {
    const r = await renderNative();
    r.fake.answers['share.pick'] = vi
      .fn()
      .mockRejectedValueOnce(new BridgeError('engine.permission_denied'))
      .mockResolvedValue(PICKED);
    await openSheetAndShare();
    expect(await within(sheet()).findByRole('alert')).toHaveTextContent(
      "Windows didn't allow isshoni to capture that.",
    );
    await userEvent.click(within(sheet()).getByRole('button', { name: 'Open Windows settings' }));
    expect(r.fake.callsOf('app.openOSSettings')).toEqual([['graphics-capture']]);
    await userEvent.click(within(sheet()).getByRole('button', { name: 'Try again' }));
    await waitFor(() => {
      expect(r.onStart).toHaveBeenCalledTimes(1);
    });
  });

  it('a start that failed is a toast where no panel shows it', async () => {
    const r = await renderNative({ onStart: () => Promise.reject(new BridgeError('publisher_offline')) });
    await openSheetAndShare();
    await waitFor(() => {
      expect(r.toasts()).toEqual(["isshoni can't reach the server to start sharing. Try again in a moment."]);
    });
    expect(r.store.getState().phase).toBe('failed');
  });
});
