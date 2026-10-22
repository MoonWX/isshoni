// Picks the Platform for this page (05 §8, boot step 2; docs/m2/03 §7.2). The desktop app's asset handler serves
// bridge.js before the module entry, which sets window.__ISSHONI_DESKTOP__; only a valid config makes the page a
// DesktopPlatform, anything else is a browser. The global name Capacitor stays reserved (types/globals.d.ts).
import { BrowserPlatform } from './browser/BrowserPlatform';
import { readDesktopConfig } from './desktop/config';
import { DesktopPlatform } from './desktop/DesktopPlatform';
import type { Platform } from './types';

export function detectPlatform(): Platform {
  const cfg = readDesktopConfig(typeof window === 'undefined' ? undefined : window.__ISSHONI_DESKTOP__);
  if (cfg) return new DesktopPlatform(cfg);
  // pending (mobile apps): window.Capacitor?.isNativePlatform?.() → new MobilePlatform()
  return new BrowserPlatform();
}
