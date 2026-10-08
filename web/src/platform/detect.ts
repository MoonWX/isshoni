// Picks the Platform for this page (05 §8, boot step 2). The global names __ISSHONI_DESKTOP__ and Capacitor are
// reserved now (types/globals.d.ts); nothing reads them in M1.
import { BrowserPlatform } from './browser/BrowserPlatform';
import type { Platform } from './types';

export function detectPlatform(): Platform {
  // later (M2/M3): window.__ISSHONI_DESKTOP__ (the Wails bridge) → new DesktopPlatform(bridge)
  //   Two transports (plan): Wails bindings, and the same-user agent relay (01 §8.14; Linux agent M4,
  //   web → desktop handoff M2).
  // pending (mobile apps): window.Capacitor?.isNativePlatform?.() → new MobilePlatform()
  return new BrowserPlatform();
}
