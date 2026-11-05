// What friends hear in the desktop ShareSheet and the first-share sheet (docs/m2/03 §10.2, §22.3 "Share machine";
// README WA77): both watch the app list while open (Audio.Watch(true) on open, false on close), the ShareSheet shows
// the one-line summary with [Details] and the volume note under Share sound, and the first-share sheet the three
// groups with the volume note, or "everything on this PC" when the engine is `endpoint`.
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import type { FriendsHear } from '../../platform/desktop/bridge';
import { createTestServices, renderRoute } from '../../test/render';
import { NativeShareButton } from './NativeShareButton';
import { appRow, friendsHear, nativeSetup, PICKED, type NativeSetup } from './testing';

const NOTE = "Muting your PC doesn't mute your stream. Use Pause or turn Share sound off.";

async function renderButton(fh: FriendsHear, confirm: ('first-share' | 'dnd')[] = []): Promise<NativeSetup> {
  const setup = await nativeSetup({ picked: { ...PICKED, confirm } });
  setup.fake.answers['audio.friendsHear'] = fh;
  const services = createTestServices({ platform: setup.platform });
  renderRoute({
    services,
    routes: [{ path: '/', element: <NativeShareButton onStart={() => Promise.resolve()} store={setup.store} /> }],
  });
  return setup;
}

const sheet = () => screen.getByRole('dialog', { name: 'Share your screen' });

describe('the desktop ShareSheet', () => {
  it('watches while open and shows "Friends will hear 2 apps · Discord kept out · [Details]"', async () => {
    const user = userEvent.setup();
    const setup = await renderButton(
      friendsHear({
        live: false,
        heard: [appRow({ id: 'exe:spotify.exe', name: 'Spotify' }), appRow({ id: 'exe:game.exe', name: 'Game' })],
      }),
    );
    await user.click(screen.getByRole('button', { name: 'Share' }));
    expect(await within(sheet()).findByText('Friends will hear 2 apps · Discord kept out')).toBeInTheDocument();
    expect(within(sheet()).getByText(NOTE)).toBeInTheDocument();
    expect(setup.fake.callsOf('audio.watch')).toEqual([[true]]);
    // Details: the groups.
    expect(within(sheet()).queryByRole('region', { name: 'Friends will hear:' })).not.toBeInTheDocument();
    await user.click(within(sheet()).getByRole('button', { name: 'Details' }));
    expect(within(sheet()).getByRole('region', { name: 'Friends will hear:' })).toBeInTheDocument();
    // A report while open replaces it.
    act(() => {
      setup.fake.emit('audio.report', friendsHear({ seq: 5, keptOut: [] }));
    });
    expect(await within(sheet()).findByText('Friends will hear 1 app')).toBeInTheDocument();
    await user.click(within(sheet()).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => {
      expect(setup.fake.callsOf('audio.watch')).toEqual([[true], [false]]);
    });
  });

  it('several kept out, nothing playing, and the endpoint engine', async () => {
    const user = userEvent.setup();
    const setup = await renderButton(
      friendsHear({
        heard: [],
        keptOut: [
          appRow({ id: 'discord', name: 'Discord', reason: 'builtin', control: 'locked' }),
          appRow({ id: 'exe:steam.exe', name: 'Steam', reason: 'mic' }),
          appRow({ id: 'exe:kook.exe', name: 'KOOK', reason: 'mic' }),
        ],
      }),
    );
    await user.click(screen.getByRole('button', { name: 'Share' }));
    expect(
      await within(sheet()).findByText('Nothing is playing right now · Discord and 2 more kept out'),
    ).toBeInTheDocument();
    act(() => {
      setup.fake.emit('audio.report', friendsHear({ seq: 9, engine: 'endpoint' }));
    });
    expect(await within(sheet()).findByText('Friends will hear everything on this PC')).toBeInTheDocument();
  });

  it('without Share sound: no summary and no volume note', async () => {
    const user = userEvent.setup();
    await renderButton(friendsHear());
    await user.click(screen.getByRole('button', { name: 'Share' }));
    await within(sheet()).findByText(NOTE);
    await user.click(within(sheet()).getByRole('checkbox', { name: 'Share sound' }));
    expect(within(sheet()).queryByText(NOTE)).not.toBeInTheDocument();
    expect(within(sheet()).queryByText(/Friends will hear/)).not.toBeInTheDocument();
  });
});

describe('the first-share sheet', () => {
  async function firstShare(fh: FriendsHear): Promise<{ setup: NativeSetup; dialog: HTMLElement }> {
    const user = userEvent.setup();
    const setup = await renderButton(fh, ['first-share']);
    await user.click(screen.getByRole('button', { name: 'Share' }));
    await user.click(within(sheet()).getByRole('button', { name: 'Share' }));
    const dialog = await screen.findByRole('dialog', { name: 'Ready to go live?' });
    return { setup, dialog };
  }

  it('shows the three groups and the volume note, and watches while open', async () => {
    const { setup, dialog } = await firstShare(
      friendsHear({
        live: false,
        notCaptured: [appRow({ id: 'exe:launcher.exe', name: 'Launcher', reason: 'launcher' })],
      }),
    );
    expect(within(dialog).getByText('Friends will see: Minecraft')).toBeInTheDocument();
    const heard = await within(dialog).findByRole('region', { name: 'Friends will hear:' });
    expect(within(heard).getByText('Spotify')).toBeInTheDocument();
    const kept = within(dialog).getByRole('region', { name: "Friends won't hear:" });
    expect(within(kept).getByText('Discord')).toBeInTheDocument();
    expect(within(kept).getByText('Voice app')).toBeInTheDocument();
    expect(within(dialog).getByRole('region', { name: 'Not captured:' })).toHaveTextContent(
      "Launcher sounds aren't shared",
    );
    expect(within(dialog).getByText(NOTE)).toBeInTheDocument();
    expect(within(dialog).getByText(/^Friends won't hear: Discord, TeamSpeak, Mumble\./)).toBeInTheDocument();
    // The ShareSheet closed when the picker answered; the first-share sheet watches now: one watch more than unwatches
    // (the bridge counts them).
    const open = (): number => setup.fake.callsOf('audio.watch').reduce((n, [on]) => n + (on === true ? 1 : -1), 0);
    await waitFor(() => {
      expect(open()).toBe(1);
    });
    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    await waitFor(() => {
      expect(open()).toBe(0);
    });
  });

  it('says "everything on this PC" for the endpoint engine', async () => {
    const { dialog } = await firstShare(friendsHear({ engine: 'endpoint' }));
    expect(await within(dialog).findByText('Friends will hear everything on this PC')).toBeInTheDocument();
    expect(within(dialog).queryByRole('region', { name: 'Friends will hear:' })).not.toBeInTheDocument();
  });
});
