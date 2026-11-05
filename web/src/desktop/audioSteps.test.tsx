// The wizard's sound and voice steps (docs/m2/03 §8.2, §8.4, §22.3 "Wizard pages"; README WA77) over the fake
// NativeBridge: after the first link (wizardDone false) the sound check runs at once and shows its result by `backend`
// and `reason` (device_busy with its text, [Open sound settings], [Try again] and [Continue]; an unknown reason with
// the generic text); the voice step calls audio.watch(true) on mount, shows voiceApps[].running and the mic toggle;
// Done sets wizardDone and goes to the room. A later server (wizardDone true) skips both.
import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createDesktopTestPlatform, type DesktopTestPlatform } from '../app/testing/desktop';
import type { SoundCheckResult } from '../platform/desktop/bridge';
import { authState } from '../platform/desktop/testing/fakeBridge';
import { friendsHear } from '../share/native/testing';
import { testOrigin } from '../test/platform';
import { SOUND_HELP } from './SoundCheck';
import { renderDesktopPage } from './testing/render';

const result = (over: Partial<SoundCheckResult>): SoundCheckResult => ({
  captured: true,
  levelDb: -23.6,
  ms: 1500,
  backend: 'process-loopback',
  ...over,
});

/** The wizard right after the link step reported linked, on a first install. */
function linkedFirstInstall(
  check: SoundCheckResult | (() => Promise<SoundCheckResult>),
  room = 'lounge',
): DesktopTestPlatform & { router: ReturnType<typeof renderDesktopPage>['router'] } {
  const d = createDesktopTestPlatform();
  d.fake.answers['auth.state'] = authState(testOrigin(), 'linked');
  d.fake.answers['app.pendingLink'] = { roomId: room };
  d.fake.answers['app.info'] = { wizardDone: false };
  d.fake.answers['audio.soundCheck'] = typeof check === 'function' ? check : () => Promise.resolve(check);
  d.fake.answers['audio.friendsHear'] = friendsHear({ live: false });
  const { router } = renderDesktopPage('/desktop/setup?link=1', d);
  return { ...d, router };
}

describe('sound step', () => {
  it('runs the check at once: "Playing a test sound…", then the good result with its level; Continue', async () => {
    const user = userEvent.setup();
    let answer: (r: SoundCheckResult) => void = () => undefined;
    const d = linkedFirstInstall(
      () =>
        new Promise<SoundCheckResult>((resolve) => {
          answer = resolve;
        }),
    );
    expect(await screen.findByRole('heading', { name: "Check your PC's sound" })).toBeInTheDocument();
    expect(await screen.findByText('Playing a test sound…')).toBeInTheDocument();
    expect(d.fake.callsOf('audio.soundCheck')).toHaveLength(1);
    act(() => {
      answer(result({}));
    });
    expect(
      await screen.findByText("✓ isshoni can capture your PC's sound, without your voice apps"),
    ).toBeInTheDocument();
    expect(screen.getByText('Measured level: -24 dB')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Try again' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    expect(await screen.findByRole('heading', { name: "What friends won't hear" })).toBeInTheDocument();
  });

  it('endpoint: voice apps can’t be kept out here, with Learn more', async () => {
    const user = userEvent.setup();
    const d = linkedFirstInstall(result({ backend: 'endpoint' }));
    expect(await screen.findByText(/can't keep voice apps out on this PC/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Learn more' }));
    expect(d.opened).toEqual([SOUND_HELP]);
  });

  it('device_busy: its text, Open sound settings, Try again and Continue', async () => {
    const user = userEvent.setup();
    const d = linkedFirstInstall(result({ captured: false, backend: 'none', reason: 'device_busy' }));
    expect(
      await screen.findByText(/Another app is using your speakers on its own \(exclusive mode\)/),
    ).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Open sound settings' }));
    expect(d.fake.callsOf('app.openOSSettings')).toEqual([['sound']]);
    await user.click(screen.getByRole('button', { name: 'Try again' }));
    await waitFor(() => {
      expect(d.fake.callsOf('audio.soundCheck')).toHaveLength(2);
    });
    expect(screen.getByRole('button', { name: 'Continue' })).toBeInTheDocument();
  });

  it.each([
    ['no_device', 'No speakers or headphones are connected. Friends hear nothing until you connect one.'],
    [
      'silent',
      "We couldn't capture the test sound, so friends would hear nothing from this PC. Friends will see your screen without sound.",
    ],
    [
      'something_new',
      "We couldn't capture the test sound, so friends would hear nothing from this PC. Friends will see your screen without sound.",
    ],
  ])('not captured, %s: its text, Try again and Continue', async (reason, text) => {
    linkedFirstInstall(result({ captured: false, backend: 'none', reason }));
    expect(await screen.findByText(text)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Continue' })).toBeInTheDocument();
  });
});

describe('voice step', () => {
  it('watches, lists the voice apps with running, the mic toggle; Done sets wizardDone and goes to the room', async () => {
    const user = userEvent.setup();
    const d = linkedFirstInstall(result({}));
    await user.click(await screen.findByRole('button', { name: 'Continue' }));
    expect(await screen.findByRole('heading', { name: "What friends won't hear" })).toBeInTheDocument();
    expect(d.fake.callsOf('audio.watch')).toEqual([[true]]);
    expect(await screen.findByText('Discord (running)')).toBeInTheDocument();
    expect(screen.getByText('TeamSpeak')).toBeInTheDocument();
    expect(screen.getByText("Apps that use your microphone (you'll be asked)")).toBeInTheDocument();
    expect(screen.getByText('isshoni itself')).toBeInTheDocument();
    expect(screen.getByText('Change this anytime in Settings → Audio.')).toBeInTheDocument();
    const toggle = screen.getByRole('switch', { name: 'Keep out apps that use my microphone' });
    expect(toggle).toBeChecked();
    await user.click(toggle);
    expect(d.fake.callsOf('audio.setKeepOutMicUsers')).toEqual([[false]]);
    await user.click(screen.getByRole('button', { name: 'Done' }));
    expect(await screen.findByRole('heading', { name: 'room lounge' })).toBeInTheDocument();
    expect(d.fake.callsOf('settings.update')).toEqual([[{ wizardDone: true }]]);
    expect(d.fake.callsOf('audio.watch')).toEqual([[true], [false]]);
  });

  it('linking the same server again (no ?link=1: the relink page’s Link again) has no audio steps', async () => {
    const d = createDesktopTestPlatform();
    d.fake.answers['auth.state'] = authState(testOrigin(), 'linked');
    d.fake.answers['app.pendingLink'] = { roomId: 'lounge' };
    d.fake.answers['app.info'] = { wizardDone: false };
    renderDesktopPage('/desktop/setup', d);
    expect(await screen.findByRole('heading', { name: 'room lounge' })).toBeInTheDocument();
    expect(d.fake.callsOf('audio.soundCheck')).toEqual([]);
  });

  it('a later server (wizardDone true) goes straight to the room', async () => {
    const d = createDesktopTestPlatform();
    d.fake.answers['auth.state'] = authState(testOrigin(), 'linked');
    d.fake.answers['app.pendingLink'] = { roomId: 'lounge' };
    d.fake.answers['app.info'] = { wizardDone: true };
    renderDesktopPage('/desktop/setup?link=1', d);
    expect(await screen.findByRole('heading', { name: 'room lounge' })).toBeInTheDocument();
    expect(d.fake.callsOf('audio.soundCheck')).toEqual([]);
  });
});
