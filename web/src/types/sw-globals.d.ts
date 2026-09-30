// Build-time constants of the service worker (05 §16.2). `build/sw-plugin.ts` injects them when it builds
// `src/sw/sw.ts` into `dist/sw.js`; only tsconfig.sw.json includes this file. It is a script (no imports or
// exports), so its declarations are global.

/** The app-shell URLs to precache: `/`, the entry chunk, its static imports and CSS, `/boot-check.js`, … */
declare const __SHELL__: readonly string[];

/** The first 12 hex digits of SHA-256 over the shell files (the same value as `shell` in `/version.json`). */
declare const __SHELL_VERSION__: string;

/** A nested group of catalog strings. */
interface PushStrings {
  readonly [key: string]: string | PushStrings;
}

/** The `push` section of `src/i18n/en.json`, for notification titles and bodies. */
declare const __PUSH_STRINGS__: PushStrings;
