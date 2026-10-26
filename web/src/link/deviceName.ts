// A device's name as the approval page and the Devices page edit it (docs/m2/02-device-auth.md §5.6): what the page
// checks before it sends one, and the text for each field code the server can answer (422 {deviceName} at
// /device/decide, {name} at PATCH /me/devices/{id}). The server normalizes the rest (trim, PRECIS Nickname).
import type { TFunction } from 'i18next';

import { FieldRequired, FieldTooLong } from '../protocol/api.gen';

/** A device name's longest length, in code points as the server counts them (02 §5.6). */
export const DEVICE_NAME_MAX = 64;

/** required or too_long for a trimmed name, else null. */
export function checkDeviceName(trimmed: string): string | null {
  if (trimmed === '') return FieldRequired;
  return Array.from(trimmed).length > DEVICE_NAME_MAX ? FieldTooLong : null;
}

/** The text under a name field for a field code: link.name.<code>, else the general one. */
export function deviceNameError(code: string, t: TFunction): string {
  switch (code) {
    case 'required':
      return t('link.name.required');
    case 'too_long':
      return t('link.name.too_long');
    case 'invalid':
      return t('link.name.invalid');
    default:
      return t('errors.validation_failed');
  }
}
