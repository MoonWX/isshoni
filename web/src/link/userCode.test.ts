// The user code helpers (docs/m2/02-device-auth.md §3): normalizeUserCode with §3's table, the same cases as Go's
// api.NormalizeUserCode tests (internal/protocol/api/device_test.go), and the formatting the page uses.
import { describe, expect, it } from 'vitest';

import { formatCodeInput, formatUserCode, normalizeUserCode, spokenUserCode } from './userCode';

describe('normalizeUserCode', () => {
  // §3's table.
  it.each(['WDJB-MJHT', 'wdjb mjht', ' wdjbmjht ', 'WDJB-MJHT-'])('%j → WDJBMJHT', (input) => {
    expect(normalizeUserCode(input)).toBe('WDJBMJHT');
  });

  it.each([
    ['A is a vowel', 'WDJB-MJHA'],
    ['7 letters', 'WDJB-MJH'],
    ['9 letters', 'WDJB-MJHTX'],
    ['an en dash', 'WDJB–MJHT'],
    ['full-width letters', 'ＷＤＪＢＭＪＨＴ'],
    ['empty', ''],
  ])('%s: invalid', (_name, input) => {
    expect(normalizeUserCode(input)).toBeNull();
  });

  // Go's extra cases: more separators, mixed case, every letter; and what is not folded.
  it.each([
    ['\tWDJB\tMJHT\t', 'WDJBMJHT'],
    ['W-D-J-B M-J-H-T', 'WDJBMJHT'],
    ['wDjB-mJhT', 'WDJBMJHT'],
    ['--WDJBMJHT--', 'WDJBMJHT'],
    ['bcdf-ghjk', 'BCDFGHJK'],
    ['LMNP-QRST', 'LMNPQRST'],
    ['VWXZ-BCDF', 'VWXZBCDF'],
    ['WDJB_MJHT', null],
    ['WDJB.MJHT', null],
    ['WDJB\nMJHT', null],
    ['WDJB\rMJHT', null],
    ['WDJB MJHT', null],
    ['WDJB‐MJHT', null],
    ['WDJB0MJH', null],
    ['WDJBMJHY', null],
    ['EIOU-WDJB', null],
    ['WDJBMJHT\u0000', null],
    ['WDJBMJH\u{1f600}', null],
    ['   ', null],
    ['--------', null],
    ['WDJBMJHTWDJBMJHT', null],
    ['WDJB-MJHT WDJB-MJHT', null],
  ])('%j → %j', (input, want) => {
    expect(normalizeUserCode(input)).toBe(want);
  });
});

describe('formatUserCode', () => {
  it('puts the "-" after the fourth letter', () => {
    expect(formatUserCode('WDJBMJHT')).toBe('WDJB-MJHT');
  });

  it.each(['', 'WDJB-MJHT', 'wdjbmjht', 'WDJBMJH', 'WDJBMJHA', 'WDJBMJHTX', 'WDJB MJH'])(
    'returns %j unchanged: not a normalized code',
    (input) => {
      expect(formatUserCode(input)).toBe(input);
    },
  );
});

describe('spokenUserCode', () => {
  it('reads the code letter by letter, with a pause between the halves (02 §10.1)', () => {
    expect(spokenUserCode('WDJBMJHT')).toBe('W D J B, M J H T');
    expect(spokenUserCode('WDJB-MJHT')).toBe('W D J B, M J H T');
  });
});

describe('formatCodeInput', () => {
  it.each([
    ['', ''],
    ['w', 'W'],
    ['wdjb', 'WDJB'],
    ['wdjbm', 'WDJB-M'],
    ['WDJB-', 'WDJB'],
    ['wdjb mjht', 'WDJB-MJHT'],
    ['WDJB-MJHT', 'WDJB-MJHT'],
    // At most 8 letters; what doesn't fit is dropped.
    ['WDJB-MJHTXYZ', 'WDJB-MJHT'],
    // Only ASCII letters stay: digits, punctuation and other scripts go.
    ['WD1JB.MJ_HT', 'WDJB-MJHT'],
    ['ＷＤＪＢ', ''],
    // Letters outside the alphabet stay visible; normalizeUserCode refuses them.
    ['aeio uwxy', 'AEIO-UWXY'],
  ])('%j → %j', (input, want) => {
    expect(formatCodeInput(input)).toBe(want);
  });

  it('gives what normalizeUserCode accepts for a valid code, however it was pasted', () => {
    for (const pasted of ['wdjbmjht', ' WDJB  MJHT ', 'W-D-J-B-M-J-H-T']) {
      expect(normalizeUserCode(formatCodeInput(pasted))).toBe('WDJBMJHT');
    }
  });
});
