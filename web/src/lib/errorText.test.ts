import { describe, expect, it } from 'vitest';

import { i18n } from '../i18n';
import en from '../i18n/en.json';
import * as apiGen from '../protocol/api.gen';
import { ApiError, NetworkError } from '../protocol/rest';
import * as typesGen from '../protocol/types.gen';
import { describeError, errorMessage, fieldErrorKey, fieldErrorMessage } from './errorText';
import { LOCAL_ERROR_CODES, LocalError, NotImplementedError } from './errors';

/** String constants of a generated module whose export name matches. */
function constants(mod: Record<string, unknown>, name: RegExp): string[] {
  return Object.entries(mod)
    .filter(([k, v]) => name.test(k) && typeof v === 'string')
    .map(([, v]) => v as string);
}

const wireCodes = constants(typesGen, /^ErrorCode[A-Z]/);
const restCodes = constants(apiGen, /^Code[A-Z]/);
const limitKinds = constants(apiGen, /^LimitKind[A-Z]/);

/** A ProtocolError as 01's errors.ts shapes it (protocol/errors.ts is S20's). */
function protocolError(code: string, extra: Record<string, unknown> = {}) {
  return Object.assign(new Error(`protocol: ${code}`), { code, scope: 'request', retryable: false, ...extra });
}

describe('i18n: every generated error code maps to an existing en.json key (05 §19.1)', () => {
  it('reads the generated constants', () => {
    expect(wireCodes.length).toBeGreaterThanOrEqual(31);
    expect(restCodes.length).toBeGreaterThanOrEqual(52);
    expect(wireCodes).toContain('protocol_unsupported');
    expect(restCodes).toContain('validation_failed');
  });

  it.each(wireCodes)('WebSocket code %s', (code) => {
    const d = describeError(protocolError(code, { retryAfterMs: 2000, params: { limit: 'rooms' } }));
    expect(d.key).not.toBe('errors.unknown');
    expect(i18n.exists(d.key, d.values)).toBe(true);
    expect(errorMessage(protocolError(code))).not.toMatch(/^errors\./);
  });

  it.each(restCodes)('REST code %s', (code) => {
    const d = describeError(new ApiError({ status: 400, code, retryAfterSec: 3, params: { limit: 'invites' } }));
    expect(d.key).not.toBe('errors.unknown');
    expect(i18n.exists(d.key, d.values)).toBe(true);
    expect(errorMessage(new ApiError({ status: 400, code }))).not.toMatch(/^errors\./);
  });

  it.each(limitKinds)('limit_reached with params.limit %s', (limit) => {
    const d = describeError(new ApiError({ status: 409, code: 'limit_reached', params: { limit } }));
    expect(d.key).toBe(`errors.limit_reached.${limit}`);
    expect(i18n.exists(d.key)).toBe(true);
  });

  it.each(LOCAL_ERROR_CODES)('client-local code %s', (code) => {
    expect(i18n.exists(`errors.local.${code}`)).toBe(true);
  });

  it('has only messages under errors (no empty or non-string leaves)', () => {
    const leaves: [string, unknown][] = [];
    const walk = (node: unknown, path: string): void => {
      if (node !== null && typeof node === 'object') {
        for (const [k, v] of Object.entries(node)) walk(v, `${path}.${k}`);
      } else {
        leaves.push([path, node]);
      }
    };
    walk(en.errors, 'errors');
    expect(leaves.filter(([, v]) => typeof v !== 'string' || v.trim() === '')).toEqual([]);
  });
});

describe('describeError / errorMessage (05 §6.3)', () => {
  it('rate_limited shows the wait in whole seconds, from either transport', () => {
    expect(errorMessage(new ApiError({ status: 429, code: 'rate_limited', retryAfterSec: 42 }))).toBe(
      'Too many attempts. Try again in 42 seconds.',
    );
    expect(errorMessage(protocolError('rate_limited', { retryAfterMs: 1200 }))).toBe(
      'Too many attempts. Try again in 2 seconds.',
    );
    expect(errorMessage(new ApiError({ status: 429, code: 'rate_limited', retryAfterSec: 1 }))).toBe(
      'Too many attempts. Try again in 1 second.',
    );
    expect(errorMessage(new ApiError({ status: 429, code: 'rate_limited' }))).toBe(
      'Too many attempts. Try again in a moment.',
    );
  });

  it('limit_reached uses params.limit, with a generic text for an unknown one', () => {
    expect(
      describeError(new ApiError({ status: 409, code: 'limit_reached', params: { limit: 'member_invites' } })).key,
    ).toBe('errors.limit_reached.member_invites');
    expect(describeError(new ApiError({ status: 409, code: 'limit_reached', params: { limit: 'future' } })).key).toBe(
      'errors.limit_reached.generic',
    );
    expect(describeError(new ApiError({ status: 409, code: 'limit_reached' })).key).toBe(
      'errors.limit_reached.generic',
    );
  });

  it('setting_locked names params.field', () => {
    expect(
      errorMessage(new ApiError({ status: 409, code: 'setting_locked', params: { field: 'registrationMode' } })),
    ).toMatch(/\(registrationMode\)/);
  });

  it('internal shows the reference: requestId (REST) or params.ref (WebSocket)', () => {
    expect(errorMessage(new ApiError({ status: 500, code: 'internal', requestId: '9f2c41d07a1be355' }))).toBe(
      'Something went wrong on the server. Reference: 9f2c41d07a1be355',
    );
    expect(errorMessage(protocolError('internal', { params: { ref: 'ab12cd34' } }))).toBe(
      'Something went wrong on the server. Reference: ab12cd34',
    );
    expect(errorMessage(new ApiError({ status: 500, code: 'internal' }))).toBe('Something went wrong on the server.');
  });

  it('an unknown code falls back to errors.unknown with the code (and the reference)', () => {
    expect(errorMessage(new ApiError({ status: 418, code: 'brand_new_code' }))).toBe(
      'Something went wrong (brand_new_code).',
    );
    expect(errorMessage(new ApiError({ status: 500, code: 'brand_new_code', requestId: 'r1' }))).toBe(
      'Something went wrong (brand_new_code). Reference: r1',
    );
    expect(errorMessage(protocolError('future_code'))).toBe('Something went wrong (future_code).');
    // Not a wire code: never used as a key.
    expect(describeError(new ApiError({ status: 400, code: 'withRef' })).key).toBe('errors.unknown');
    expect(describeError(new ApiError({ status: 400, code: 'local' })).key).toBe('errors.unknown');
    // A non-JSON error body.
    expect(errorMessage(new ApiError({ status: 421, code: 'unknown' }))).toBe('Something went wrong (unknown).');
  });

  it('local errors use errors.local.<code>', () => {
    expect(describeError(new NetworkError()).key).toBe('errors.local.offline');
    expect(describeError(new LocalError('capture_failed')).key).toBe('errors.local.capture_failed');
    expect(describeError(protocolError('connection_lost', { local: true })).key).toBe('errors.local.connection_lost');
    expect(describeError(protocolError('not_ready', { local: true })).key).toBe('errors.local.not_ready');
    // The same code on the wire (04's 503 not_ready) is a different message.
    expect(describeError(new ApiError({ status: 503, code: 'not_ready' })).key).toBe('errors.not_ready');
    expect(describeError(protocolError('weird', { local: true })).key).toBe('errors.unknown');
  });

  it('anything else is errors.unknown', () => {
    expect(describeError(new Error('boom'))).toEqual({ key: 'errors.unknown', values: { code: 'unknown' } });
    expect(describeError(new NotImplementedError('sharing.pick', 'S35')).key).toBe('errors.unknown');
    expect(describeError('string')).toEqual({ key: 'errors.unknown', values: { code: 'unknown' } });
  });

  it('field errors use fieldErrors.<field>.<code>, else errors.validation_failed', () => {
    expect(fieldErrorKey('username', 'too_short')).toBe('fieldErrors.username.too_short');
    expect(fieldErrorMessage('username', 'too_short')).toBe('Check the highlighted fields.');
    i18n.addResource('en', 'translation', 'fieldErrors.username.too_short', 'Too short.');
    try {
      expect(fieldErrorMessage('username', 'too_short')).toBe('Too short.');
    } finally {
      i18n.removeResourceBundle('en', 'translation');
      i18n.addResourceBundle('en', 'translation', en);
    }
  });
});
