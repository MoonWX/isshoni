// Settings → Audio (docs/m2/03 §14.4, §22.3 "Settings"; README WA77): watched while open; the built-in list as locked
// text ("Always kept out: Discord, TeamSpeak, …"), the mic toggle, the per-app decisions from Audio.AppRules with
// Change (Audio.SetAppDecision, auto forgets), the built-in voice apps listed as locked, the ducking tip's button, and
// Run the sound check again; no sound-mode control.
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createDesktopTestPlatform } from '../app/testing/desktop';
import type { AppRule } from '../platform/desktop/bridge';
import { appRow, friendsHear } from '../share/native/testing';
import { renderDesktopPage } from './testing/render';

const RULES: AppRule[] = [
  { id: 'exe:kook.exe', name: 'KOOK', decision: 'keep-out' },
  { id: 'exe:obs64.exe', name: 'OBS Studio', decision: 'include' },
];

function renderSettings(rules: AppRule[] = RULES) {
  const d = createDesktopTestPlatform();
  d.fake.answers['audio.friendsHear'] = friendsHear({ live: false });
  d.fake.answers['audio.appRules'] = rules;
  d.fake.answers['audio.soundCheck'] = {
    captured: true,
    levelDb: -20,
    ms: 1500,
    backend: 'process-loopback',
  };
  const r = renderDesktopPage('/desktop/settings', d);
  return { ...r, d };
}

describe('Settings → Audio', () => {
  it('shows the locked built-in list, the mic toggle and the per-app decisions; watches while open', async () => {
    const { d, unmount } = renderSettings();
    const section = await screen.findByRole('region', { name: 'Audio' });
    expect(section).toHaveAttribute('id', 'audio');
    expect(await within(section).findByText('Always kept out: Discord, TeamSpeak, Mumble')).toBeInTheDocument();
    expect(within(section).getByRole('switch', { name: 'Keep out apps that use my microphone' })).toBeChecked();
    expect(await within(section).findByRole('combobox', { name: 'Choice for KOOK' })).toHaveValue('keep-out');
    expect(within(section).getByRole('combobox', { name: 'Choice for OBS Studio' })).toHaveValue('include');
    // The built-in voice apps are listed as locked: no control of theirs.
    const locked = section.querySelectorAll('[data-locked="true"]');
    expect([...locked].map((li) => li.textContent)).toEqual([
      'DiscordAlways kept out (voice app)',
      'TeamSpeakAlways kept out (voice app)',
      'MumbleAlways kept out (voice app)',
    ]);
    // No sound-mode control: no "All sound" or mode choice anywhere.
    expect(within(section).queryByText(/all sound/i)).not.toBeInTheDocument();
    expect(within(section).queryByRole('radio')).not.toBeInTheDocument();
    expect(within(section).getAllByRole('combobox')).toHaveLength(2);
    expect(d.fake.callsOf('audio.watch')).toEqual([[true]]);
    unmount();
    expect(d.fake.callsOf('audio.watch')).toEqual([[true], [false]]);
  });

  it('Change sets a decision, auto forgets it; the mic toggle calls setKeepOutMicUsers', async () => {
    const user = userEvent.setup();
    const { d } = renderSettings();
    const kook = await screen.findByRole('combobox', { name: 'Choice for KOOK' });
    await user.selectOptions(kook, 'Included');
    await user.selectOptions(screen.getByRole('combobox', { name: 'Choice for OBS Studio' }), 'Decide for me');
    await waitFor(() => {
      expect(d.fake.callsOf('audio.setAppDecision')).toEqual([
        ['exe:kook.exe', 'include'],
        ['exe:obs64.exe', 'auto'],
      ]);
    });
    // The rules are read again after each change.
    await waitFor(() => {
      expect(d.fake.callsOf('audio.appRules').length).toBeGreaterThanOrEqual(3);
    });
    await user.click(screen.getByRole('switch', { name: 'Keep out apps that use my microphone' }));
    expect(d.fake.callsOf('audio.setKeepOutMicUsers')).toEqual([[false]]);
  });

  it('reads the rules again when a report changes a decision, not on every report', async () => {
    const { d } = renderSettings();
    await screen.findByRole('combobox', { name: 'Choice for KOOK' });
    await waitFor(() => {
      expect(d.fake.callsOf('audio.friendsHear')).toHaveLength(1);
    });
    const kook = appRow({ id: 'exe:kook.exe', name: 'KOOK', decision: 'auto' });
    act(() => {
      d.fake.emit('audio.report', friendsHear({ seq: 10, live: true, heard: [kook] }));
    });
    const base = d.fake.callsOf('audio.appRules').length;
    // Levels and playing changes alone (05 sends up to 4 reports a second during a share): no new read.
    for (let seq = 11; seq < 15; seq += 1) {
      act(() => {
        d.fake.emit('audio.report', friendsHear({ seq, live: true, heard: [{ ...kook, playing: seq % 2 === 0 }] }));
      });
    }
    expect(d.fake.callsOf('audio.appRules')).toHaveLength(base);
    // A decision made elsewhere (a prompt kept KOOK out): read again, once; so does the mic toggle's change.
    const keptOut = [appRow({ ...kook, decision: 'keep-out', reason: 'user' })];
    act(() => {
      d.fake.emit('audio.report', friendsHear({ seq: 15, live: true, heard: [], keptOut }));
    });
    await waitFor(() => {
      expect(d.fake.callsOf('audio.appRules')).toHaveLength(base + 1);
    });
    act(() => {
      d.fake.emit('audio.report', friendsHear({ seq: 16, live: true, heard: [], keptOut, keepOutMicUsers: false }));
    });
    await waitFor(() => {
      expect(d.fake.callsOf('audio.appRules')).toHaveLength(base + 2);
    });
  });

  it('without decisions says so; the ducking tip opens the Communications tab; the sound check runs again', async () => {
    const user = userEvent.setup();
    const { d } = renderSettings([]);
    expect(await screen.findByText(/You haven't made a choice for an app yet/)).toBeInTheDocument();
    expect(screen.getByText(/Windows turns other sounds down during calls/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Open sound settings' }));
    expect(d.fake.callsOf('app.openOSSettings')).toEqual([['sound-communications']]);
    await user.click(screen.getByRole('button', { name: 'Run the sound check again' }));
    expect(
      await screen.findByText("✓ isshoni can capture your PC's sound, without your voice apps"),
    ).toBeInTheDocument();
    expect(d.fake.callsOf('audio.soundCheck')).toHaveLength(1);
  });
});
