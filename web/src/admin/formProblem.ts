// What an admin form shows after a failed change (05 §6.3): the server sends codes, never English.
// - validation_failed (422) names fields: each code shows under its control. A field with a text of its own in
//   fieldErrors.<field>.<code> (username, serverName) uses it; the others get the admin pages' text for the code,
//   and the numbers that go with it (a range, a length) are in the hint under the control;
// - some codes are about one field without naming it: wrong_password is the admin's own password,
//   username_taken and room_name_taken the name that was typed. The form says which (codeFields);
// - everything else is one message above the form, from errors.<code> (lib/errorText.ts).
// The checks a form runs before it sends anything return the same field codes, so both read the same texts.
import type { TFunction } from 'i18next';

import { i18n } from '../i18n';
import { errorMessage, fieldErrorKey } from '../lib/errorText';
import {
  CodeValidationFailed,
  FieldInvalid,
  FieldNotAllowed,
  FieldOutOfRange,
  FieldRequired,
  FieldReserved,
  FieldTooLong,
  FieldTooShort,
} from '../protocol/api.gen';
import { ApiError } from '../protocol/rest';

/** Field name → field code (03 §12.2), for the fields that failed. */
export type FieldCodes<F extends string> = Partial<Record<F, string>>;

/** What a form shows: a translated message per field, and one above the form. */
export interface FormProblem<F extends string> {
  readonly fields: Partial<Record<F, string>>;
  readonly form: string | null;
}

/** Error code → the form's field that the code is about (wrong_password → currentPassword). */
export type CodeFields<F extends string> = Readonly<Partial<Record<string, F>>>;

export function noProblem<F extends string>(): FormProblem<F> {
  return { fields: {}, form: null };
}

/** The text for a field code where the field has none of its own. Codes this build doesn't know get the generic one. */
function codeMessage(code: string, t: TFunction): string {
  switch (code) {
    case FieldRequired:
      return t('admin.fieldErrors.required');
    case FieldTooShort:
      return t('admin.fieldErrors.too_short');
    case FieldTooLong:
      return t('admin.fieldErrors.too_long');
    case FieldInvalid:
      return t('admin.fieldErrors.invalid');
    case FieldOutOfRange:
      return t('admin.fieldErrors.out_of_range');
    case FieldNotAllowed:
      return t('admin.fieldErrors.not_allowed');
    case FieldReserved:
      return t('admin.fieldErrors.reserved');
    default:
      return t('errors.validation_failed');
  }
}

/** The message for one field code: fieldErrors.<field>.<code> when en.json has it, else the text for the code. */
export function fieldMessage(field: string, code: string, t: TFunction): string {
  const key = fieldErrorKey(field, code);
  return i18n.exists(key) ? t(key) : codeMessage(code, t);
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

/** The messages for the codes of a check the form ran itself. */
export function problemFromCodes<F extends string>(codes: FieldCodes<F>, t: TFunction): FormProblem<F> {
  const fields: Partial<Record<F, string>> = {};
  for (const field of Object.keys(codes) as F[]) {
    const code = codes[field];
    if (code !== undefined) fields[field] = fieldMessage(field, code, t);
  }
  return { fields, form: null };
}

/**
 * What to show for a failed request. `fields` are the form's own field names (the API's JSON names): a
 * validation_failed that names only other fields falls back to the message above the form.
 */
export function problemFromError<F extends string>(
  err: unknown,
  fields: readonly F[],
  codeFields: CodeFields<F>,
  t: TFunction,
): FormProblem<F> {
  if (!(err instanceof ApiError)) return { fields: {}, form: errorMessage(err, t) };

  if (err.code === CodeValidationFailed) {
    const out: Partial<Record<F, string>> = {};
    for (const field of fields) {
      const code = err.fields?.[field];
      if (code !== undefined) out[field] = fieldMessage(field, code, t);
    }
    if (Object.keys(out).length > 0) return { fields: out, form: null };
    return { fields: {}, form: errorMessage(err, t) };
  }

  const field = Object.hasOwn(codeFields, err.code) ? codeFields[err.code] : undefined;
  if (field !== undefined && fields.includes(field)) {
    const out: Partial<Record<F, string>> = {};
    out[field] = errorMessage(err, t);
    return { fields: out, form: null };
  }

  return { fields: {}, form: errorMessage(err, t) };
}

/** Characters as the server counts them (runes, that is code points), not UTF-16 units. */
export function runeLength(value: string): number {
  return Array.from(value).length;
}
