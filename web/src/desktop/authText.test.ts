// The texts of the wizard's and the relink page's outcomes (docs/m2/03 §8.1–§8.3): every probe and server-change
// code, the password form's REST codes, the link notices and 02 §13.3's version lines have a real text.
import { describe, expect, it } from 'vitest';

import '../i18n/lazy/desktop';
import { initI18n } from '../i18n';
import { compatText, failureOf, failureText, fieldTexts, relinkText, type RelinkNotice } from './authText';

const i18n = initI18n();
const t = i18n.t.bind(i18n);

function text(code: string, params: Record<string, unknown> = {}): string {
  return failureText(t, { code, params }, '0.2.0');
}

describe('failureText', () => {
  it.each([
    ['invalid_address', "That doesn't look like a server address. Check it and try again."],
    ['server_unreachable', "isshoni can't reach this server. Check the address and your connection."],
    ['tls_untrusted', "This server's certificate isn't trusted, so isshoni won't connect to it."],
    ['not_isshoni', "That address isn't an isshoni server."],
    ['server_not_set_up', "This server isn't set up yet. Its admin has to finish the setup in a browser first."],
    ['too_many_servers', "You've added 20 servers. Remove one first."],
    ['not_found', "That isn't there any more."],
    ['busy', 'Stop sharing first.'],
    ['rate_limited', 'Too many tries. Wait a moment and try again.'],
    ['not_linked', 'Link this app to the server first.'],
    ['invalid_argument', "isshoni couldn't do that: something it was given isn't valid."],
    ['internal', 'Something went wrong in isshoni. Try again.'],
    ['invalid_credentials', 'Wrong username or password.'],
    ['account_pending', "An admin hasn't approved your account yet. Try logging in later."],
    ['account_disabled', 'Your account was disabled. Ask an admin.'],
    ['server_busy', 'The server is busy. Try again in a moment.'],
    ['validation_failed', 'Check the highlighted fields.'],
    ['something_new', "Couldn't start linking with this server"],
  ])('%s', (code, want) => {
    expect(text(code)).toBe(want);
  });

  it('rate_limited with the server’s wait', () => {
    expect(text('rate_limited', { retryAfter: 1 })).toBe('Too many attempts. Try again in 1 second.');
    expect(text('rate_limited', { retryAfter: 2.2 })).toBe('Too many attempts. Try again in 3 seconds.');
  });

  it('incompatible_server by its verdict (02 §13.3)', () => {
    expect(text('incompatible_server', { verdict: 'update_app', serverVersion: '0.3.0' })).toBe(
      'This server needs a newer isshoni app. You have 0.2.0; the server runs 0.3.0.',
    );
    expect(text('incompatible_server', { verdict: 'update_server', serverVersion: '0.1.4' })).toBe(
      'This server runs isshoni 0.1.4, which is too old for this app. Ask your admin to update it.',
    );
    expect(compatText(t, { verdict: 'update_server', serverVersion: '' }, '0.2.0')).toContain('isshoni ?');
  });

  it('no text is a bare key', () => {
    for (const code of ['invalid_address', 'invalid_credentials', 'busy', 'x']) {
      expect(text(code)).not.toMatch(/^(desktop\.)?errors\./);
    }
  });
});

describe('failureOf', () => {
  it('reads a BridgeError by shape, and calls anything else internal', () => {
    expect(failureOf({ code: 'busy', params: { a: 1 } })).toEqual({ code: 'busy', params: { a: 1 } });
    expect(failureOf(new Error('x'))).toEqual({ code: 'internal', params: {} });
    expect(failureOf(null)).toEqual({ code: 'internal', params: {} });
  });
});

describe('fieldTexts', () => {
  it('gives M1’s field text for validation_failed’s fields, and nothing otherwise', () => {
    const f = fieldTexts(t, { code: 'validation_failed', params: { fields: { username: 'too_long', other: 'x' } } });
    expect(Object.keys(f)).toEqual(['username']);
    expect(f.username).not.toMatch(/^fieldErrors\./);
    expect(fieldTexts(t, { code: 'invalid_credentials', params: {} })).toEqual({});
  });
});

describe('relinkText', () => {
  it.each<[RelinkNotice, string]>([
    ['signed_out', 'This PC was signed out of Friends (removed on the Devices page, or not used for 90 days)'],
    [
      'refresh_reused',
      "Someone else used this PC's sign-in, so Friends signed it out. Link again and check your Devices page",
    ],
    ['account_disabled', 'Your account on Friends is disabled'],
  ])('%s', (notice, want) => {
    expect(relinkText(t, notice, 'Friends')).toBe(want);
  });
});
