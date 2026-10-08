// Error → message (05 §6.3). The server sends codes, never English: every WebSocket code (01 §12.1, ErrorCode in
// types.gen.ts) and every REST code (03 §12.2 with 04's rows, Code… in api.gen.ts) has an `errors.<code>` text in
// en.json, which `check:i18n` enforces. Client-local codes use `errors.local.<code>`.
//
// Special cases:
// - rate_limited shows the wait in seconds: ProtocolError.retryAfterMs / 1000 or ApiError.retryAfterSec;
// - limit_reached uses errors.limit_reached.<params.limit> (rooms, invites, member_invites, pending_signups);
// - setting_locked names params.field;
// - internal (and any code shown through the fallback) adds the reference: params.ref (WebSocket) or requestId
//   (REST);
// - an unknown code falls back to errors.unknown with the code.
import type { TFunction } from 'i18next';

import { i18n } from '../i18n';
import { ApiError, NetworkError } from '../protocol/rest';
import { LocalError, LOCAL_ERROR_CODES } from './errors';
import { secondsFromMs } from './time';

/** An i18n key with its interpolation values, and the reference to show with it. */
export interface ErrorDescription {
  readonly key: string;
  readonly values: Readonly<Record<string, unknown>>;
  /** The server's error reference (params.ref or requestId), shown after the message. */
  readonly ref?: string;
}

/**
 * The fields of 01's ProtocolError (protocol/errors.ts) that messages use. Read structurally, so this module works
 * with any error that has them.
 */
interface ProtocolErrorLike {
  readonly code: string;
  readonly scope: string;
  readonly local?: boolean;
  readonly retryAfterMs?: number;
  readonly params?: Readonly<Record<string, unknown>>;
}

function isProtocolErrorLike(err: unknown): err is ProtocolErrorLike {
  if (typeof err !== 'object' || err === null) return false;
  const e = err as Record<string, unknown>;
  return typeof e['code'] === 'string' && typeof e['scope'] === 'string';
}

const LOCAL = new Set<string>(LOCAL_ERROR_CODES);
const WIRE_CODE = /^[a-z][a-z0-9_]*$/;

/** Whether en.json has a message (a string, or plural forms) at this key. */
function hasMessage(key: string): boolean {
  const lng = i18n.resolvedLanguage ?? i18n.language;
  const res = (k: string): unknown => i18n.getResource(lng, 'translation', k);
  return typeof res(key) === 'string' || typeof res(`${key}_other`) === 'string';
}

function stringParam(params: Readonly<Record<string, unknown>> | undefined, name: string): string | undefined {
  const v = params?.[name];
  return typeof v === 'string' && v !== '' ? v : undefined;
}

function withRef(d: Omit<ErrorDescription, 'ref'>, ref: string | undefined): ErrorDescription {
  return ref === undefined ? d : { ...d, ref };
}

/** A wire code (WebSocket or REST) with its details → key and values. */
function describeCode(
  code: string,
  params: Readonly<Record<string, unknown>> | undefined,
  retryAfterSec: number | undefined,
  ref: string | undefined,
): ErrorDescription {
  switch (code) {
    case 'rate_limited':
      return retryAfterSec !== undefined && retryAfterSec > 0
        ? { key: 'errors.rate_limited.wait', values: { count: Math.ceil(retryAfterSec) } }
        : { key: 'errors.rate_limited.later', values: {} };
    case 'limit_reached': {
      const limit = stringParam(params, 'limit');
      const key = limit !== undefined ? `errors.limit_reached.${limit}` : '';
      return hasMessage(key) ? { key, values: {} } : { key: 'errors.limit_reached.generic', values: {} };
    }
    case 'setting_locked':
      return { key: 'errors.setting_locked', values: { field: stringParam(params, 'field') ?? '' } };
  }
  // Wire codes are snake_case (01 §12.1, 03 §12.2); anything else can't be one of ours, and must not reach a
  // template like errors.withRef.
  const key = `errors.${code}`;
  if (WIRE_CODE.test(code) && hasMessage(key)) return withRef({ key, values: { code } }, ref);
  return withRef({ key: 'errors.unknown', values: { code } }, ref);
}

/**
 * Describes an error for the UI: ApiError and NetworkError (REST), 01's ProtocolError (by its fields), LocalError;
 * anything else is errors.unknown.
 */
export function describeError(err: unknown): ErrorDescription {
  if (err instanceof NetworkError) return { key: 'errors.local.offline', values: {} };
  if (err instanceof LocalError) return { key: `errors.local.${err.code}`, values: {} };
  if (err instanceof ApiError) {
    return describeCode(err.code, err.params, err.retryAfterSec, err.requestId);
  }
  if (isProtocolErrorLike(err)) {
    if (err.local === true) {
      return LOCAL.has(err.code)
        ? { key: `errors.local.${err.code}`, values: {} }
        : { key: 'errors.unknown', values: { code: err.code } };
    }
    const retryAfterSec = err.retryAfterMs !== undefined ? secondsFromMs(err.retryAfterMs) : undefined;
    return describeCode(err.code, err.params, retryAfterSec, stringParam(err.params, 'ref'));
  }
  return { key: 'errors.unknown', values: { code: 'unknown' } };
}

/** The message for an error, in the current language, with its reference when there is one. */
export function errorMessage(err: unknown, t: TFunction = i18n.t): string {
  const d = describeError(err);
  const message = t(d.key, d.values);
  return d.ref === undefined ? message : t('errors.withRef', { message, ref: d.ref });
}

/**
 * The key of a validation_failed field code (03 §12.2): fieldErrors.<field>.<code>. Forms fall back to
 * errors.validation_failed when their namespace has no text for it.
 */
export function fieldErrorKey(field: string, code: string): string {
  return `fieldErrors.${field}.${code}`;
}

/** The message for one field of a validation_failed ApiError, falling back to errors.validation_failed. */
export function fieldErrorMessage(field: string, code: string, t: TFunction = i18n.t): string {
  const key = fieldErrorKey(field, code);
  return hasMessage(key) ? t(key) : t('errors.validation_failed');
}
