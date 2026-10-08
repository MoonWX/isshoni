// What an admin form shows for a failed change (05 §6.3): which codes land under a field, which above the form.
import { describe, expect, it } from 'vitest';

import { i18n } from '../i18n';
import { ApiError, NetworkError } from '../protocol/rest';
import { fieldCodes, fieldMessage, problemFromCodes, problemFromError, runeLength } from './formProblem';

const t = i18n.t;

function apiError(status: number, code: string, extra: Partial<ConstructorParameters<typeof ApiError>[0]> = {}) {
  return new ApiError({ status, code, ...extra });
}

describe('fieldMessage', () => {
  it("uses the field's own text when en.json has one", () => {
    expect(fieldMessage('username', 'reserved', t)).toBe('That username is reserved. Pick another.');
    expect(fieldMessage('serverName', 'too_long', t)).toBe("That's too long for a name.");
  });

  it.each([
    ['required', "This can't be empty."],
    ['too_short', "That's too short."],
    ['too_long', "That's too long."],
    ['invalid', "That value can't be used."],
    ['out_of_range', 'That number is outside the allowed range.'],
    ['not_allowed', "That isn't allowed here."],
    ['reserved', 'That name is reserved. Pick another.'],
  ])('falls back to the text for the code %s', (code, shown) => {
    expect(fieldMessage('maxSharesPerRoom', code, t)).toBe(shown);
  });

  it('falls back to the general text for a code this build does not know', () => {
    expect(fieldMessage('name', 'too_shiny', t)).toBe('Check the highlighted fields.');
    // Never a group of messages, never a path out of the field's own texts.
    expect(fieldMessage('username', '', t)).toBe('Check the highlighted fields.');
    expect(fieldMessage('fieldErrors', 'username', t)).toBe('Check the highlighted fields.');
  });
});

describe('problemFromCodes', () => {
  it('gives each failed field its message, and nothing above the form', () => {
    const codes = fieldCodes<'name' | 'note'>({ name: 'required', note: null });
    expect(codes).toEqual({ name: 'required' });
    expect(problemFromCodes(codes, t)).toEqual({ fields: { name: "This can't be empty." }, form: null });
  });
});

describe('problemFromError', () => {
  it("puts a 422's field codes under the form's own fields", () => {
    const err = apiError(422, 'validation_failed', { fields: { name: 'too_long', other: 'invalid' } });
    expect(problemFromError(err, ['name'], {}, t)).toEqual({ fields: { name: "That's too long." }, form: null });
  });

  it('shows a 422 that names only other fields above the form', () => {
    const err = apiError(422, 'validation_failed', { fields: { other: 'invalid' } });
    expect(problemFromError(err, ['name'], {}, t)).toEqual({ fields: {}, form: 'Check the highlighted fields.' });
    expect(problemFromError(apiError(422, 'validation_failed'), [], {}, t).form).toBe('Check the highlighted fields.');
  });

  it('puts a code that is about one field under it', () => {
    const codeFields = { wrong_password: 'currentPassword', username_taken: 'username' } as const;
    expect(problemFromError(apiError(403, 'wrong_password'), ['username', 'currentPassword'], codeFields, t)).toEqual({
      fields: { currentPassword: "Your current password isn't right." },
      form: null,
    });
    // The form has no such field: above the form.
    expect(problemFromError(apiError(409, 'username_taken'), ['currentPassword'], codeFields, t)).toEqual({
      fields: {},
      form: 'That username is taken.',
    });
  });

  it('is not fooled by a code named like an object property', () => {
    expect(problemFromError(apiError(400, 'constructor'), ['name'], {}, t)).toEqual({
      fields: {},
      form: 'Something went wrong (constructor).',
    });
  });

  it('shows everything else above the form, with its details', () => {
    expect(problemFromError(apiError(409, 'last_admin'), [], {}, t).form).toBe(
      'The server needs at least one active admin.',
    );
    expect(problemFromError(apiError(429, 'rate_limited', { retryAfterSec: 12 }), [], {}, t).form).toBe(
      'Too many attempts. Try again in 12 seconds.',
    );
    expect(problemFromError(apiError(409, 'limit_reached', { params: { limit: 'rooms' } }), [], {}, t).form).toBe(
      'This server has as many rooms as it can hold. Delete one first.',
    );
    expect(problemFromError(apiError(500, 'internal', { requestId: 'req-1' }), [], {}, t).form).toBe(
      'Something went wrong on the server. Reference: req-1',
    );
    expect(problemFromError(new NetworkError(), [], {}, t).form).toBe("You're offline. Check your connection.");
    expect(problemFromError(new Error('boom'), [], {}, t).form).toBe('Something went wrong (unknown).');
  });
});

describe('runeLength', () => {
  it('counts characters as the server does: code points, not UTF-16 units', () => {
    expect(runeLength('')).toBe(0);
    expect(runeLength('abc')).toBe(3);
    expect(runeLength('太郎')).toBe(2);
    expect(runeLength('🎬')).toBe(1);
  });
});
