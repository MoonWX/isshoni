// STUB until S38 (05 W11 PWA shell) replaces this file.
//
// Contract for the replacement (BrowserPlatform and main.tsx import these, so the exports and signatures stay):
// - createPwaProvider(): the PwaProvider of 05 §8: install state and prompt (§16.3 "Install prompts"), onUpdateReady
//   when registration.waiting appears, applyUpdate() posts SKIP_WAITING and reloads on controllerchange (§16.2).
// - registerServiceWorker(): boot step 6 (05 §4) calls it once, after the first render, when the browser is idle,
//   and only in production builds: navigator.serviceWorker.register('/sw.js', {scope: '/', updateViaCache: 'none'}).
//
// Until then there is no service worker (build/sw-plugin.ts is a placeholder too): nothing to register, no install
// prompt, no update ever becomes ready.
import { NotImplementedError } from '../../lib/errors';
import type { PwaProvider } from '../types';

export function createPwaProvider(): PwaProvider {
  return {
    installState: () => 'none',
    promptInstall: () => Promise.resolve('unavailable'),
    onUpdateReady: () => () => undefined,
    applyUpdate: () => {
      throw new NotImplementedError('pwa.applyUpdate', 'S38');
    },
  };
}

/** Registers the service worker. The stub does nothing: there is no /sw.js before S38. */
export function registerServiceWorker(): Promise<void> {
  return Promise.resolve();
}
