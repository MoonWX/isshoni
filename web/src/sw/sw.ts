// The service worker's entry (05 §16.2). Hand-written rather than Workbox: it only caches the app shell for an
// offline start and handles push.
//
// build/sw-plugin.ts builds this file after the main build into dist/sw.js, a classic script (an IIFE: module
// service workers aren't universal), and replaces the three constants of src/types/sw-globals.d.ts. The server
// serves /sw.js at the root with `no-cache` and the HTML CSP (04 §9.5–9.6); platform/browser/pwa.ts registers it in
// production builds only.
//
// worker.ts has the handlers, routes.ts the fetch strategy, shell.ts the cache, push.ts the notifications.
import { startWorker } from './worker';

startWorker(self as unknown as ServiceWorkerGlobalScope, {
  shell: __SHELL__,
  shellVersion: __SHELL_VERSION__,
  pushStrings: __PUSH_STRINGS__,
});
