// i18n setup (05 §16.5): the English catalog, one i18next namespace (`translation`), nested keys.
//
// Catalog rules (the catalog files are shared; README "Shared files and their owners"):
// - Top-level namespaces: common, auth, setup, conntest, room, viewer, share, account, admin, doctor, fix, errors
//   (+ errors.local), fieldErrors, push, a11y. Each slice adds keys under its own namespaces only, in the file that
//   has the namespace.
// - The catalog is split by who downloads it:
//     en.json             the main bundle: every namespace that code in the entry chunk uses, and those that
//                         could be lazy but are not yet (05 §16.5 lists them, and what each still needs);
//     lazy/<ns>.en.json   a namespace that only lazy page folders use (account, admin, setup), with the key paths
//                         it has in the code (`{"admin": {…}}`). It is in the chunk of the folders that use it.
//   A page folder whose pages use a lazy namespace imports lazy/<ns>.ts in its entry module (admin/index.ts:
//   `import '../i18n/lazy/admin';`). That module adds the namespace to i18next when the folder's chunk runs, which
//   is before the router has the page (05 §5), so no page renders a bare key. Code of the entry chunk must not use
//   a lazy namespace: `npm run check:i18n` follows the imports and fails when a key could be asked for before its
//   namespace is there.
//   To make a namespace lazy: move it from en.json to lazy/<ns>.en.json, add lazy/<ns>.ts next to it (three lines,
//   see lazy/admin.ts), and import that from the entry module of every folder that uses the namespace.
// - Plurals use i18next suffixes: viewer.watching_one / viewer.watching_other, called as t('viewer.watching', {count}).
// - Write every key in full at the call site (no `keyPrefix`): `npm run check:i18n` reads t('…') literals and fails
//   when a key is missing from the catalog.
// - Dates and numbers use Intl with i18n.language.
import i18next, { type i18n } from 'i18next';
import { initReactI18next } from 'react-i18next';

import en from './en.json';

/** The only language in M1; others are *later*. */
export const DEFAULT_LANGUAGE = 'en';

/** The one i18next namespace: the catalog's own namespaces are the first segment of a key, not i18next's. */
const NAMESPACE = 'translation';

/**
 * The shape of en.json, the main bundle's part of the catalog, for code that needs a typed view of it (e.g. the
 * service worker's push strings).
 */
export type Catalog = typeof en;

/** A part of the catalog: messages under their full key paths, top-level namespace first, as in en.json. */
export interface Messages {
  readonly [key: string]: string | Messages;
}

/**
 * i18next's own copy of messages. It keeps the objects it is given and merges later additions into them, which
 * would change the imported catalog files for everything else that reads them.
 */
function copyOf<T extends Messages>(messages: T): T {
  return JSON.parse(JSON.stringify(messages)) as T;
}

/** Added before initI18n() ran; it takes them in. */
const early: Messages[] = [];

/**
 * Adds messages to the catalog; lazy/<ns>.ts calls it with its namespace when a lazy folder's chunk runs. Messages
 * that are there already are replaced, so adding the same ones twice changes nothing. Components that are mounted
 * don't render again for it: a namespace is added before the first page that uses it renders.
 */
export function addMessages(messages: Messages): void {
  if (!i18next.isInitialized) {
    early.push(messages);
    return;
  }
  // deep: merged into the namespace's messages; overwrite: an existing key takes the new text.
  i18next.addResourceBundle(DEFAULT_LANGUAGE, NAMESPACE, copyOf(messages), true, true);
}

/**
 * Every lazy namespace, for Vitest: a test renders a page without its folder's entry module, so nothing would add
 * the page's namespace. Only initI18n's test branch calls this, and a build has no such branch (see there).
 */
function lazyNamespaces(): Messages[] {
  return Object.values(import.meta.glob<Messages>('./lazy/*.en.json', { eager: true, import: 'default' }));
}

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
      resources: { [DEFAULT_LANGUAGE]: { [NAMESPACE]: copyOf(en) } },
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
    // Written as this very comparison on purpose: Vite puts the mode in at build time, the bundler then drops the
    // branch, and with it the lazy catalogs leave the entry chunk (app/chunks.node.test.ts checks that they do).
    if (import.meta.env.MODE === 'test') {
      for (const messages of lazyNamespaces()) addMessages(messages);
    }
    for (const messages of early.splice(0)) addMessages(messages);
  }
  if (typeof document !== 'undefined') {
    document.documentElement.lang = i18next.resolvedLanguage ?? DEFAULT_LANGUAGE;
  }
  return i18next;
}

export { i18next as i18n };
