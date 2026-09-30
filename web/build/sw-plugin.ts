// Service-worker build (05 §16.2). PLACEHOLDER until S38 (05 W11, PWA shell) replaces this file.
//
// S38's plugin, after the main build: builds src/sw/sw.ts as a separate IIFE into dist/sw.js with the build-time
// constants of src/types/sw-globals.d.ts: __SHELL__ and __SHELL_VERSION__ from build/shell.ts (resolveShell over
// shellUrls, so they match version.json's `shell`), and __PUSH_STRINGS__ from the `push` section of src/i18n/en.json.
// Its closeBundle hook must be `sequential: true`, so compress-plugin (order: post) compresses sw.js afterwards.
//
// Until then there is no service worker: this plugin does nothing, and vite.config.ts already lists it.
import type { Plugin } from 'vite';

export function swPlugin(): Plugin {
  return { name: 'isshoni:sw', apply: 'build' };
}
