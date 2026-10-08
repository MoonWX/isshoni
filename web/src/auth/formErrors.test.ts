// Field checks and the error → message mapping of the auth and setup forms (05 §6.3, 03 §7.1–§7.2, §12.2).
import { describe, expect, it } from 'vitest';

import { i18n } from '../i18n';
import en from '../i18n/en.json';
import { LocalError } from '../lib/errors';
import { fieldErrorMessage } from '../lib/errorText';
import {
  FieldInvalid,
  FieldNotAllowed,
  FieldOutOfRange,
  FieldRequired,
  FieldReserved,
  FieldSameAsUsername,
  FieldTooCommon,
  FieldTooLong,
  FieldTooShort,
} from '../protocol/api.gen';
import { ApiError, NetworkError } from '../protocol/rest';
import {
  checkNewPassword,
  checkServerName,
  checkUsername,
  DEFAULT_ACCOUNT_RULES,
  fieldCodes,
  problemFromCodes,
  problemFromError,
  SERVER_NAME_MAX_LENGTH,
  waitMessage,
} from './formErrors';
import { SessionNotKeptError } from './session';

const rules = DEFAULT_ACCOUNT_RULES;
const t = i18n.t.bind(i18n);

describe('the checks before a request', () => {
  it('the default rules are 03 §7.1–§7.2: usernames 2–32, passwords 8–128', () => {
    expect(rules).toEqual({
      usernameMinLength: 2,
      usernameMaxLength: 32,
      passwordMinLength: 8,
      passwordMaxLength: 128,
    });
  });

  it.each([
    ['', FieldRequired],
    ['   ', FieldRequired],
    ['a', FieldTooShort],
    [' a ', FieldTooShort],
    ['ab', null],
    ['太郎', null],
    ['राजे', null],
    ['x'.repeat(32), null],
    ['x'.repeat(33), FieldTooLong],
    // 32 characters outside the BMP: 64 UTF-16 units, still within the rule.
    ['𝒳'.repeat(32), null],
    // The rest (allowed characters, reserved names) is the server's call.
    ['-sam', null],
    ['isshoni', null],
  ])('checkUsername(%j) → %j', (value, want) => {
    expect(checkUsername(value, rules)).toBe(want);
  });

  it.each([
    ['', FieldRequired],
    ['1234567', FieldTooShort],
    ['12345678', null],
    ['        ', null],
    ['パスワードです八', null],
    ['p'.repeat(128), null],
    ['p'.repeat(129), FieldTooLong],
    ['password', null],
  ])('checkNewPassword(%j) → %j', (value, want) => {
    expect(checkNewPassword(value, rules)).toBe(want);
  });

  it('checkNewPassword follows the rules /info gives, not a built-in number', () => {
    const stricter = { ...rules, passwordMinLength: 15 };
    expect(checkNewPassword('12345678901234', stricter)).toBe(FieldTooShort);
    expect(checkNewPassword('123456789012345', stricter)).toBeNull();
  });

  it('checkServerName: optional, at most 64 characters', () => {
    expect(SERVER_NAME_MAX_LENGTH).toBe(64);
    expect(checkServerName('')).toBeNull();
    expect(checkServerName('n'.repeat(64))).toBeNull();
    expect(checkServerName(`  ${'n'.repeat(64)}  `)).toBeNull();
    expect(checkServerName('n'.repeat(65))).toBe(FieldTooLong);
  });

  it('fieldCodes keeps only the fields that failed', () => {
    expect(fieldCodes({ username: FieldRequired, password: null })).toEqual({ username: 'required' });
    expect(fieldCodes({ username: null, password: null })).toEqual({});
  });
});

describe('the field texts (fieldErrors.<field>.<code>)', () => {
  it('has a text for every code the server gives the auth fields (03 §7.1–§7.2, §9)', () => {
    const expected: Record<string, string[]> = {
      username: [FieldRequired, FieldTooShort, FieldTooLong, FieldInvalid, FieldReserved],
      password: [FieldRequired, FieldTooShort, FieldTooLong, FieldInvalid, FieldTooCommon, FieldSameAsUsername],
      serverName: [FieldTooLong, FieldInvalid],
    };
    for (const [field, codes] of Object.entries(expected)) {
      const texts = (en.fieldErrors as Record<string, Record<string, string>>)[field] ?? {};
      expect(Object.keys(texts).sort()).toEqual([...codes].sort());
      for (const code of codes) {
        const text = fieldErrorMessage(field, code, t);
        expect(text).toBe(texts[code]);
        expect(text).not.toBe(en.errors.validation_failed);
      }
    }
  });

  it('carries no placeholders: lib/errorText.ts shows these texts without values (the hints have the numbers)', () => {
    const walk = (node: unknown): string[] =>
      typeof node === 'string' ? [node] : Object.values(node as Record<string, unknown>).flatMap(walk);
    for (const text of walk(en.fieldErrors)) expect(text).not.toMatch(/[{}]/);
  });

  it.each([
    ['username', FieldOutOfRange],
    ['username', FieldNotAllowed],
    ['password', 'a_code_from_a_newer_server'],
    ['nickname', FieldRequired],
    ['username', ''],
  ])('a pair without a text falls back to the general one: %s / %j', (field, code) => {
    expect(problemFromCodes({ [field]: code }, t).fields).toEqual({ [field]: 'Check the highlighted fields.' });
  });
});

describe('problemFromError', () => {
  const fields = ['username', 'password'] as const;
  const problem = (err: unknown) => problemFromError(err, fields, t);

  it('validation_failed: a text per field of the form', () => {
    const err = new ApiError({
      status: 422,
      code: 'validation_failed',
      fields: { username: 'reserved', password: 'too_common' },
    });
    expect(problem(err)).toEqual({
      fields: {
        username: 'That username is reserved. Pick another.',
        password: "That password is too common. Pick one that's harder to guess.",
      },
      form: null,
    });
  });

  it('validation_failed without a field of the form: the general message', () => {
    for (const fieldsOfError of [{ serverName: 'too_long' }, undefined]) {
      const err = new ApiError({
        status: 422,
        code: 'validation_failed',
        ...(fieldsOfError ? { fields: fieldsOfError } : {}),
      });
      expect(problem(err)).toEqual({ fields: {}, form: 'Check the highlighted fields.' });
    }
  });

  it('username_taken: under the username when the form has one, above the form otherwise', () => {
    const err = new ApiError({ status: 409, code: 'username_taken' });
    expect(problem(err)).toEqual({ fields: { username: 'That username is taken.' }, form: null });
    expect(problemFromError(err, ['password'] as const, t)).toEqual({
      fields: {},
      form: 'That username is taken.',
    });
  });

  it.each([
    ['rate_limited', 429, 42, 'Too many attempts. Try again in 42 seconds.'],
    ['rate_limited', 429, 0.2, 'Too many attempts. Try again in 1 second.'],
    ['server_busy', 503, 5, 'The server is busy. Try again in 5 seconds.'],
    ['server_busy', 503, 1, 'The server is busy. Try again in 1 second.'],
  ] as const)('%s with a wait of %d s', (code, status, retryAfterSec, text) => {
    const err = new ApiError({ status, code, retryAfterSec });
    expect(problem(err)).toEqual({ fields: {}, form: text, wait: { code, seconds: Math.ceil(retryAfterSec) } });
  });

  it('rate_limited and server_busy without a wait: no countdown', () => {
    expect(problem(new ApiError({ status: 429, code: 'rate_limited' }))).toEqual({
      fields: {},
      form: 'Too many attempts. Try again in a moment.',
    });
    expect(problem(new ApiError({ status: 503, code: 'server_busy', retryAfterSec: 0 }))).toEqual({
      fields: {},
      form: 'The server is busy. Try again in a moment.',
    });
  });

  it('everything else: errors.<code> above the form', () => {
    expect(problem(new ApiError({ status: 401, code: 'invalid_credentials' })).form).toBe(
      'Wrong username or password.',
    );
    expect(problem(new ApiError({ status: 500, code: 'internal', requestId: 'r1' })).form).toBe(
      'Something went wrong on the server. Reference: r1',
    );
    expect(problem(new NetworkError()).form).toBe("You're offline. Check your connection.");
    expect(problem(new LocalError('offline')).form).toBe("You're offline. Check your connection.");
    expect(problem(new Error('boom')).form).toBe('Something went wrong (unknown).');
    expect(problem(new SessionNotKeptError()).form).toMatch(/didn't keep you signed in/);
  });

  it('problemFromCodes: the same texts for the form’s own checks', () => {
    expect(problemFromCodes({ username: 'too_short', password: 'required' }, t)).toEqual({
      fields: { username: "That's too short for a username.", password: 'Enter a password.' },
      form: null,
    });
  });

  it('waitMessage counts in whole seconds, with plural forms', () => {
    expect(waitMessage(t, 'rate_limited', 2)).toBe('Too many attempts. Try again in 2 seconds.');
    expect(waitMessage(t, 'rate_limited', 1)).toBe('Too many attempts. Try again in 1 second.');
    expect(waitMessage(t, 'server_busy', 1)).toBe('The server is busy. Try again in 1 second.');
  });
});
