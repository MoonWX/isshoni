// i18n setup (05 §16.5): one bundled English catalog, one namespace, nested keys.
//
// Catalog rules (en.json is shared; README "Shared files and their owners"):
// - Top-level namespaces: common, auth, setup, conntest, room, viewer, share, account, admin, doctor, fix, errors
//   (+ errors.local), fieldErrors, push, a11y. Each slice adds keys under its own namespaces only.
// - Plurals use i18next suffixes: viewer.watching_one / viewer.watching_other, called as t('viewer.watching', {count}).
// - Write every key in full at the call site (no `keyPrefix`): `npm run check:i18n` reads t('…') literals and fails
//   when a key is missing from en.json.
// - Dates and numbers use Intl with i18n.language.
import i18next, { type i18n } from 'i18next';
import { initReactI18next } from 'react-i18next';

import en from './en.json';

/** The only language in M1; others are *later*. */
export const DEFAULT_LANGUAGE = 'en';

/** The catalog's shape, for code that needs a typed view of en.json (e.g. the service worker's push strings). */
export type Catalog = typeof en;

/**
 * Initializes the shared i18next instance synchronously (the catalog is bundled) and sets `<html lang>`. Boot step 3
 * (05 §4) calls it before the first render; the Vitest setup file calls it too. Calling it again is a no-op that
 * returns the same instance.
 */
export function initI18n(): i18n {
  if (!i18next.isInitialized) {
    // With inline resources and initAsync: false, init() finishes before it returns; the promise it also returns
    // carries nothing more.
    void i18next.use(initReactI18next).init({
      resources: { [DEFAULT_LANGUAGE]: { translation: en } },
      lng: DEFAULT_LANGUAGE,
      fallbackLng: DEFAULT_LANGUAGE,
      supportedLngs: [DEFAULT_LANGUAGE],
      initAsync: false,
      // Keys are dotted paths only; ':' is never a namespace separator (one namespace).
      nsSeparator: false,
      returnEmptyString: false,
      // React escapes text; escaping here too would show "&amp;" to users.
      interpolation: { escapeValue: false },
    });
  }
  if (typeof document !== 'undefined') {
    document.documentElement.lang = i18next.resolvedLanguage ?? DEFAULT_LANGUAGE;
  }
  return i18next;
}

export { i18next as i18n };
