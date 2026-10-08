// What a form shows after a failed submit (05 §6.3, §15.1): the server sends codes, never English.
// - validation_failed (422) names fields: each code becomes fieldErrors.<field>.<code> under its control
//   (lib/errorText.ts fieldErrorMessage; a pair without a text of its own gets errors.validation_failed);
// - username_taken (409) is about one field too, so it shows under the username;
// - rate_limited (429) and server_busy (503) carry a wait in seconds, which the form counts down (useRetryWait.ts);
// - everything else is one message above the form, from errors.<code> (lib/errorText.ts).
// The same field codes come from the checks the form runs before it sends anything (the rules of /info's
// accountRules), so both sources read the same texts. Those texts carry no numbers: the limits are in the hint
// under each field, which comes from /info (AccountFields.tsx).
import type { TFunction } from 'i18next';

import { errorMessage, fieldErrorMessage } from '../lib/errorText';
import {
  CodeRateLimited,
  CodeServerBusy,
  CodeUsernameTaken,
  CodeValidationFailed,
  FieldRequired,
  FieldTooLong,
  FieldTooShort,
  type AccountRules,
  type Field,
} from '../protocol/api.gen';
import { ApiError } from '../protocol/rest';
import { SessionNotKeptError } from './session';

/** The rules of 03 §7.1–§7.2, used only if /info somehow isn't loaded; /info is the source (03 §12.4.1). */
export const DEFAULT_ACCOUNT_RULES: AccountRules = {
  usernameMinLength: 2,
  usernameMaxLength: 32,
  passwordMinLength: 8,
  passwordMaxLength: 128,
};

/** The serverName setting holds at most 64 characters (03 §9). */
export const SERVER_NAME_MAX_LENGTH = 64;

/** Field name → field code (03 §12.2), for the fields that failed. */
export type FieldCodes<F extends string> = Partial<Record<F, string>>;

/** What a form shows: a translated message per field, and one above the form. */
export interface FormProblem<F extends string> {
  readonly fields: Partial<Record<F, string>>;
  readonly form: string | null;
  /** rate_limited or server_busy with a wait: the code and the seconds to count down. */
  readonly wait?: { readonly code: typeof CodeRateLimited | typeof CodeServerBusy; readonly seconds: number };
}

export function noProblem<F extends string>(): FormProblem<F> {
  return { fields: {}, form: null };
}

/**
 * Characters as the server counts them (03 §7.1–§7.2: runes, that is code points), not UTF-16 units: "太郎" is 2.
 * Deliberately not grapheme clusters (Intl.Segmenter): the server would count a flag emoji as 2 as well.
 */
function length(value: string): number {
  return Array.from(value).length;
}

/** required, too_short or too_long by the account rules, else null. The server checks the rest (03 §7.1). */
export function checkUsername(value: string, rules: AccountRules): Field | null {
  const name = value.trim();
  if (name === '') return FieldRequired;
  if (length(name) < rules.usernameMinLength) return FieldTooShort;
  if (length(name) > rules.usernameMaxLength) return FieldTooLong;
  return null;
}

/** required, too_short or too_long by the account rules, else null. The server checks the rest (03 §7.2). */
export function checkNewPassword(value: string, rules: AccountRules): Field | null {
  if (value === '') return FieldRequired;
  if (length(value) < rules.passwordMinLength) return FieldTooShort;
  if (length(value) > rules.passwordMaxLength) return FieldTooLong;
  return null;
}

/** too_long when the optional server name is over 64 characters, else null. */
export function checkServerName(value: string): Field | null {
  return length(value.trim()) > SERVER_NAME_MAX_LENGTH ? FieldTooLong : null;
}

/** Drops the fields without a code. */
export function fieldCodes<F extends string>(codes: Partial<Record<F, string | null>>): FieldCodes<F> {
  const out: FieldCodes<F> = {};
  for (const field of Object.keys(codes) as F[]) {
    const code = codes[field];
    if (typeof code === 'string') out[field] = code;
  }
  return out;
}

/** The messages for the codes of a client-side check. */
export function problemFromCodes<F extends string>(codes: FieldCodes<F>, t: TFunction): FormProblem<F> {
  const fields: Partial<Record<F, string>> = {};
  for (const field of Object.keys(codes) as F[]) {
    const code = codes[field];
    if (code !== undefined) fields[field] = fieldErrorMessage(field, code, t);
  }
  return { fields, form: null };
}

/**
 * What to show for a failed request. `fields` are the form's own field names (the API's JSON names): a
 * validation_failed that names only other fields falls back to the message above the form.
 */
export function problemFromError<F extends string>(err: unknown, fields: readonly F[], t: TFunction): FormProblem<F> {
  if (err instanceof SessionNotKeptError) return { fields: {}, form: t('auth.sessionNotKept') };
  if (!(err instanceof ApiError)) return { fields: {}, form: errorMessage(err, t) };

  if (err.code === CodeValidationFailed) {
    const out: Partial<Record<F, string>> = {};
    for (const field of fields) {
      const code = err.fields?.[field];
      if (code !== undefined) out[field] = fieldErrorMessage(field, code, t);
    }
    return Object.keys(out).length > 0 ? { fields: out, form: null } : { fields: {}, form: errorMessage(err, t) };
  }

  const username = fields.find((f) => f === 'username');
  if (err.code === CodeUsernameTaken && username !== undefined) {
    const out: Partial<Record<F, string>> = {};
    out[username] = errorMessage(err, t);
    return { fields: out, form: null };
  }

  if ((err.code === CodeRateLimited || err.code === CodeServerBusy) && (err.retryAfterSec ?? 0) > 0) {
    const seconds = Math.ceil(err.retryAfterSec ?? 0);
    return { fields: {}, form: waitMessage(t, err.code, seconds), wait: { code: err.code, seconds } };
  }

  return { fields: {}, form: errorMessage(err, t) };
}

/** "Too many attempts. Try again in 42 seconds." / "The server is busy. Try again in 5 seconds." */
export function waitMessage(
  t: TFunction,
  code: typeof CodeRateLimited | typeof CodeServerBusy,
  seconds: number,
): string {
  return code === CodeServerBusy
    ? t('auth.serverBusyWait', { count: seconds })
    : t('errors.rate_limited.wait', { count: seconds });
}
