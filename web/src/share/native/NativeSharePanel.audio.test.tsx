// What friends hear in the native share panel (docs/m2/03 §10.3, §22.3 "Native panel"; README WA77): rows from
// FriendsHear in three groups, Keep out/Include only for `control: 'toggle'`, the locked shared voice app's warning,
// reasons (incl. `tree` with `detail`) and warnings by key, tips with dismissTip, levels only while the expander is
// open (the watchLevels count), banners from FriendsHear.banners, the mic prompt card's text and buttons by kind with
// decide(id, 'keep-out' | 'include') on the buttons and dismissPrompt(id) on ✕ and Esc (never decide(id, 'auto')),
// no "No sound captured" line however long the mix is silent, and "Nothing is playing right now" while sound is on
// and no heard row plays, gone when one plays.
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import type { FriendsHear, Levels, ShareView } from '../../platform/desktop/bridge';
import { shareView } from '../../platform/desktop/testing/shareView';
import { createTestServices, renderRoute } from '../../test/render';
import { NativeSharePanel } from './NativeSharePanel';
import { appRow, friendsHear, nativeSetup, type NativeSetup } from './testing';

async function renderPanel(fh: FriendsHear | null, current: ShareView = shareView()): Promise<NativeSetup> {
  const setup = await nativeSetup({ current });
  if (fh !== null) setup.fake.answers['audio.friendsHear'] = fh;
  const services = createTestServices({ platform: setup.platform });
  renderRoute({
    services,
    routes: [
      {
        path: '/',
        element: <NativeSharePanel share={setup.sharing.current()} onStop={() => undefined} store={setup.store} />,
      },
    ],
  });
  return setup;
}

const region = () => screen.getByRole('region', { name: 'Your share' });

async function openExpander(user: ReturnType<typeof userEvent.setup>): Promise<HTMLElement> {
  await user.click(within(region()).getByRole('button', { name: 'Share settings' }));
  return await screen.findByRole('heading', { name: 'What friends hear' });
}

const levels = (apps: Levels['apps'], mix = -12): Levels => ({ seq: 1, mixPeakDb: mix, mixRmsDb: mix - 3, apps });

describe('What friends hear in the expander', () => {
  it('lists the three groups with reasons, warnings and the toggles of toggle rows only', async () => {
    const user = userEvent.setup();
    const fh = friendsHear({
      heard: [
        appRow({ id: 'exe:eldenring.exe', name: 'ELDEN RING', shared: true }),
        appRow({ id: 'exe:kook.exe', name: 'KOOK', usesMic: true, decision: 'include', warning: 'included-uses-mic' }),
      ],
      keptOut: [
        appRow({ id: 'discord', name: 'Discord', reason: 'builtin', control: 'locked' }),
        appRow({ id: 'exe:obs.exe', name: 'OBS', reason: 'user', decision: 'keep-out' }),
      ],
      notCaptured: [appRow({ id: 'exe:helper.exe', name: 'Helper', reason: 'tree', detail: 'Discord' })],
    });
    const setup = await renderPanel(fh);
    await openExpander(user);

    const heard = screen.getByRole('region', { name: 'Friends will hear:' });
    const kept = screen.getByRole('region', { name: "Friends won't hear:" });
    const not = screen.getByRole('region', { name: 'Not captured:' });
    expect(
      within(heard)
        .getAllByRole('listitem')
        .map((li) => li.dataset['appId']),
    ).toEqual(['exe:eldenring.exe', 'exe:kook.exe']);
    expect(
      within(heard).getByText('KOOK uses your microphone and friends hear it, because you chose Include.'),
    ).toBeInTheDocument();
    expect(within(kept).getByText('Voice app')).toBeInTheDocument();
    expect(within(kept).getByText('You kept it out')).toBeInTheDocument();
    expect(within(not).getByText('Started Discord, which is kept out')).toBeInTheDocument();
    // Discord is locked: no button; OBS can be included; heard rows can be kept out.
    const discord = within(kept).getByText('Discord').closest('li');
    expect(discord).not.toBeNull();
    expect(within(discord as HTMLElement).queryByRole('button')).not.toBeInTheDocument();
    await user.click(within(kept).getByRole('button', { name: 'Include' }));
    await user.click(within(heard).getAllByRole('button', { name: 'Keep out' })[0] as HTMLElement);
    expect(setup.fake.callsOf('audio.setAppDecision')).toEqual([
      ['exe:obs.exe', 'include'],
      ['exe:eldenring.exe', 'keep-out'],
    ]);
  });

  it('a locked voice app that is the shared app says it is always kept out; the warnings by key', async () => {
    const user = userEvent.setup();
    await renderPanel(
      friendsHear({
        heard: [appRow({ id: 'exe:valorant.exe', name: 'VALORANT', shared: true, warning: 'shared-uses-mic' })],
        keptOut: [
          appRow({ id: 'discord', name: 'Discord', reason: 'builtin', control: 'locked', shared: true }),
          appRow({ id: 'exe:game.exe', name: 'Game', reason: 'user', shared: true, warning: 'shared-kept-out' }),
          appRow({ id: 'exe:x.exe', name: 'X', reason: 'pid-reuse', detail: 'Discord' }),
        ],
      }),
    );
    await openExpander(user);
    for (const text of [
      'Discord is a voice app, so its sound is always kept out.',
      "VALORANT is the app you're sharing and it uses your microphone. Friends will hear its voice chat.",
      "You're sharing Game, but its sound is kept out.",
      'Not shared, to be safe: Windows may count Discord as part of it',
    ]) {
      expect(screen.getByText(text)).toBeInTheDocument();
    }
  });

  it('levels only while the expander is open: watchLevels on, the rows’ bars and the mix meter, then off', async () => {
    const user = userEvent.setup();
    const setup = await renderPanel(friendsHear());
    expect(setup.fake.callsOf('audio.watchLevels')).toEqual([]);
    await openExpander(user);
    expect(setup.fake.callsOf('audio.watchLevels')).toEqual([[true]]);
    act(() => {
      setup.fake.emit('audio.levels', levels({ 'exe:spotify.exe': { peakDb: -18.5, source: 'captured' } }));
    });
    const bar = await screen.findByRole('meter', { name: 'Spotify: sound level' });
    expect(bar).toHaveAttribute('value', '-18.5');
    expect(bar).toHaveAttribute('min', '-60');
    expect(bar).toHaveAttribute('max', '0');
    expect(screen.getByRole('meter', { name: 'Sound level of your stream' })).toHaveAttribute('value', '-12');
    await user.click(within(region()).getByRole('button', { name: 'Share settings' }));
    expect(setup.fake.callsOf('audio.watchLevels')).toEqual([[true], [false]]);
    expect(setup.fake.listeners('audio.levels')).toBe(0);
  });

  it('the ducking tip: Open sound settings, and its dismiss', async () => {
    const user = userEvent.setup();
    const setup = await renderPanel(friendsHear({ tips: [{ code: 'ducking' }] }));
    await openExpander(user);
    expect(screen.getByText(/Windows turns other sounds down during calls/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Open sound settings' }));
    await user.click(screen.getByRole('button', { name: "Don't show again" }));
    expect(setup.fake.callsOf('app.openOSSettings')).toEqual([['sound-communications']]);
    expect(setup.fake.callsOf('audio.dismissTip')).toEqual([['ducking']]);
  });

  it('"Nothing is playing right now" while sound is on and no heard row plays, gone when one plays; never "No sound captured"', async () => {
    const user = userEvent.setup();
    const quiet = friendsHear({ seq: 2, heard: [appRow({ id: 'exe:spotify.exe', name: 'Spotify', playing: false })] });
    const setup = await renderPanel(quiet);
    await openExpander(user);
    expect(screen.getByText('Nothing is playing right now')).toBeInTheDocument();
    // However long the mix stays silent, M1's line never shows.
    act(() => {
      setup.fake.emit('audio.levels', levels({}, -120));
    });
    expect(screen.queryByText(/No sound captured/)).not.toBeInTheDocument();
    act(() => {
      setup.fake.emit('audio.report', friendsHear({ seq: 3 }));
    });
    await waitFor(() => {
      expect(screen.queryByText('Nothing is playing right now')).not.toBeInTheDocument();
    });
  });

  it('the volume note under Share sound', async () => {
    const user = userEvent.setup();
    await renderPanel(friendsHear());
    await openExpander(user);
    expect(
      screen.getByText("Muting your PC doesn't mute your stream. Use Pause or turn Share sound off."),
    ).toBeInTheDocument();
  });

  it('a share without sound shows no list and no note', async () => {
    const user = userEvent.setup();
    await renderPanel(friendsHear({ mode: 'off' }), shareView({ withAudio: false, audioOn: false }));
    await user.click(within(region()).getByRole('button', { name: 'Share settings' }));
    expect(screen.queryByRole('heading', { name: 'What friends hear' })).not.toBeInTheDocument();
    expect(screen.queryByText(/Muting your PC/)).not.toBeInTheDocument();
  });
});

describe('banners and the mic prompt card', () => {
  it('shows the banners from FriendsHear.banners, always, by code', async () => {
    await renderPanel(
      friendsHear({
        banners: [
          { code: 'exclusion-unavailable', level: 'error' },
          { code: 'sound-off', level: 'info', params: { cause: 'paused' } },
          { code: 'capture-trouble', level: 'warning', params: { app: 'Spotify' } },
        ],
      }),
    );
    expect(await screen.findByRole('alert')).toHaveTextContent(
      "Voice apps can't be kept out on this PC, so friends hear everything it plays, including your calls.",
    );
    expect(screen.getByText('Sharing is paused. Friends hear nothing.')).toBeInTheDocument();
    expect(screen.getByText("Friends can't hear Spotify right now. isshoni keeps trying.")).toBeInTheDocument();
  });

  it('a mic prompt: its question and buttons by kind; the buttons call decide', async () => {
    const user = userEvent.setup();
    const setup = await renderPanel(
      friendsHear({
        prompts: [
          { id: 'p1', appId: 'exe:kook.exe', name: 'KOOK', kind: 'mic', since: 1 },
          { id: 'p2', appId: 'exe:chrome.exe', name: 'Google Chrome', kind: 'browser-mic', since: 2 },
        ],
      }),
    );
    const mic = await screen.findByRole('group', {
      name: 'KOOK is using your microphone. Keep it out of your stream?',
    });
    const browser = screen.getByRole('group', { name: /Your browser is using the mic \(Discord Web\?\)/ });
    await user.click(within(mic).getByRole('button', { name: 'Keep out' }));
    await user.click(within(browser).getByRole('button', { name: 'Include' }));
    expect(within(browser).queryByRole('button', { name: 'Keep out' })).not.toBeInTheDocument();
    expect(setup.fake.callsOf('audio.decide')).toEqual([
      ['p1', 'keep-out'],
      ['p2', 'include'],
    ]);
    // Answered: the cards go at once.
    expect(screen.queryByRole('group', { name: /KOOK is using/ })).not.toBeInTheDocument();
  });

  it('✕ and Esc dismiss a prompt (dismissPrompt), never decide(id, "auto")', async () => {
    const user = userEvent.setup();
    const setup = await renderPanel(
      friendsHear({
        prompts: [
          { id: 'p1', appId: 'exe:kook.exe', name: 'KOOK', kind: 'mic', since: 1 },
          { id: 'p2', appId: 'exe:steam.exe', name: 'Steam', kind: 'mic', since: 2 },
        ],
      }),
    );
    const first = await screen.findByRole('group', { name: /KOOK is using/ });
    await user.click(within(first).getByRole('button', { name: 'Dismiss' }));
    const second = screen.getByRole('group', { name: /Steam is using/ });
    within(second).getByRole('button', { name: 'Keep out' }).focus();
    await user.keyboard('{Escape}');
    expect(setup.fake.callsOf('audio.dismissPrompt')).toEqual([['p1'], ['p2']]);
    expect(setup.fake.callsOf('audio.decide')).toEqual([]);
    expect(screen.queryByRole('group', { name: /is using your microphone/ })).not.toBeInTheDocument();
  });
});
