// The Settings page's rules without the page (03 §9): what a save sends, and what it refuses to send.
import { describe, expect, it } from 'vitest';

import type { Settings } from '../protocol/api.gen';
import {
  hasChanges,
  NUMBER_RANGES,
  parseNumberSetting,
  SETTING_NAMES,
  settingsChanges,
  type NumberSetting,
  type SettingsEdits,
} from './settingsForm';
import { DEFAULT_SETTINGS } from './testing/harness';

const SETTINGS: Settings = { ...DEFAULT_SETTINGS, serverName: 'Movie club', maxSharesPerRoom: 4 };

function changes(edits: SettingsEdits, locked: string[] = [], settings: Settings = SETTINGS) {
  return settingsChanges(edits, settings, locked);
}

describe('settingsChanges', () => {
  it('covers every setting but setupWizardDone', () => {
    const all = Object.keys(DEFAULT_SETTINGS).filter((name) => name !== 'setupWizardDone');
    expect([...SETTING_NAMES].sort()).toEqual(all.sort());
  });

  it('sends nothing without edits', () => {
    expect(changes({})).toEqual({ patch: {}, errors: {} });
    expect(hasChanges(changes({}))).toBe(false);
  });

  it('sends only the fields whose value differs from the server', () => {
    const c = changes({
      serverName: 'Film club',
      // Typed, then typed back.
      maxSharesPerRoom: '4',
      membersCanInvite: true,
      updateCheck: true,
      registrationMode: 'invite',
      transferAlertGb: '500',
    });
    expect(c).toEqual({ patch: { serverName: 'Film club', membersCanInvite: true, transferAlertGb: 500 }, errors: {} });
    expect(hasChanges(c)).toBe(true);
  });

  it('reads the same value in another spelling as no change', () => {
    expect(changes({ serverName: '  Movie club  ', maxSharesPerRoom: ' 004 ', minClientVersion: '  ' }).patch).toEqual(
      {},
    );
  });

  it('resets a field by sending its default, never null', () => {
    const c = changes({ serverName: '', maxSharesPerRoom: '0' });
    expect(c.patch).toEqual({ serverName: '', maxSharesPerRoom: 0 });
    expect(JSON.stringify(c.patch)).not.toContain('null');
  });

  it('never sends a locked field, whatever was edited', () => {
    const c = changes(
      {
        registrationMode: 'closed',
        maxSharesPerRoom: '9',
        updateCheck: false,
        minClientVersion: 'nonsense',
        serverName: 'x',
      },
      ['registrationMode', 'maxSharesPerRoom', 'updateCheck', 'minClientVersion'],
    );
    // minClientVersion's bad value is no error either: the field can't be sent.
    expect(c).toEqual({ patch: { serverName: 'x' }, errors: {} });
  });

  it('never sends setupWizardDone', () => {
    const edits = { setupWizardDone: false } as SettingsEdits;
    expect(changes(edits).patch).toEqual({});
  });

  it('holds back the fields that can not be sent as they are, with their field code', () => {
    const c = changes({
      inviteDefaultTtlHours: '',
      inviteDefaultMaxUses: 'ten',
      maxParticipantsPerRoom: '10001',
      maxShareBitrateKbps: '100',
      minClientVersion: 'v1.2',
      serverName: 'x'.repeat(65),
      transferAlertGb: '12',
    });
    expect(c.errors).toEqual({
      inviteDefaultTtlHours: 'required',
      inviteDefaultMaxUses: 'invalid',
      maxParticipantsPerRoom: 'out_of_range',
      maxShareBitrateKbps: 'out_of_range',
      minClientVersion: 'invalid',
      serverName: 'too_long',
    });
    expect(c.patch).toEqual({ transferAlertGb: 12 });
    expect(hasChanges(c)).toBe(true);
  });

  it('counts the server name in characters, like the server', () => {
    expect(changes({ serverName: '🎬'.repeat(64) }).errors).toEqual({});
    expect(changes({ serverName: '🎬'.repeat(65) }).errors).toEqual({ serverName: 'too_long' });
  });

  it.each(['1.2.3', '0.2.0', '1.0.0-rc.1', '1.0.0+build.5', ''])('takes %j as a minimum version', (v) => {
    const c = changes({ minClientVersion: v }, [], { ...SETTINGS, minClientVersion: '9.9.9' });
    expect(c).toEqual({ patch: { minClientVersion: v }, errors: {} });
  });

  it.each(['1.2', 'v1.2.3', '1.2.3.4', '01.2.3', 'latest'])('refuses %j as a minimum version', (v) => {
    expect(changes({ minClientVersion: v }).errors).toEqual({ minClientVersion: 'invalid' });
  });
});

describe('parseNumberSetting', () => {
  it.each(Object.entries(NUMBER_RANGES) as [NumberSetting, (typeof NUMBER_RANGES)[NumberSetting]][])(
    '%s takes its range and nothing outside it',
    (name, { min, max, zeroToo }) => {
      expect(parseNumberSetting(name, String(min))).toBe(min);
      expect(parseNumberSetting(name, String(max))).toBe(max);
      expect(parseNumberSetting(name, String(max + 1))).toBe('out_of_range');
      if (min > 0) {
        expect(parseNumberSetting(name, String(min - 1))).toBe(min - 1 === 0 && zeroToo === true ? 0 : 'out_of_range');
        expect(parseNumberSetting(name, '0')).toBe(zeroToo === true ? 0 : 'out_of_range');
      }
    },
  );

  it('takes whole numbers only', () => {
    expect(parseNumberSetting('maxSharesPerRoom', ' 12 ')).toBe(12);
    expect(parseNumberSetting('maxSharesPerRoom', '')).toBe('required');
    expect(parseNumberSetting('maxSharesPerRoom', '   ')).toBe('required');
    for (const text of ['1.5', '-1', '1e3', '0x10', 'ten', '1 000', '١٢']) {
      expect(parseNumberSetting('maxSharesPerRoom', text)).toBe('invalid');
    }
    // Too many digits for any range, and for a number that JSON could carry exactly.
    expect(parseNumberSetting('transferAlertGb', '9'.repeat(30))).toBe('out_of_range');
  });

  it('lets the bitrate cap be 0 or 500 and up, nothing between', () => {
    expect(parseNumberSetting('maxShareBitrateKbps', '0')).toBe(0);
    expect(parseNumberSetting('maxShareBitrateKbps', '499')).toBe('out_of_range');
    expect(parseNumberSetting('maxShareBitrateKbps', '500')).toBe(500);
  });
});
