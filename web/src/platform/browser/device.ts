// Device facts for BrowserPlatform (05 §8): feature detection first; UA parsing (lib/ua.ts) only where there is no
// feature to detect (phone or tablet, iOS for PiP and help text).
import { isIOS, isMobileUA, type UAData } from '../../lib/ua';

/** The browser facts device detection reads; deviceEnv() takes them from the globals, tests pass their own. */
export interface DeviceEnv {
  readonly ua: string;
  readonly uaData?: UAData;
  /** navigator.platform ("MacIntel" on iPadOS as on a Mac). */
  readonly platform: string;
  readonly maxTouchPoints: number;
  /** window.matchMedia(query).matches; false where matchMedia is missing. */
  readonly matches: (query: string) => boolean;
}

export function deviceEnv(): DeviceEnv {
  const nav = globalThis.navigator;
  const uaData = nav.userAgentData;
  return {
    ua: nav.userAgent,
    ...(uaData ? { uaData } : {}),
    platform: nav.platform,
    maxTouchPoints: nav.maxTouchPoints,
    matches: (q) => (typeof globalThis.matchMedia === 'function' ? globalThis.matchMedia(q).matches : false),
  };
}

export function isIOSDevice(env: DeviceEnv = deviceEnv()): boolean {
  return isIOS(env.ua, env.platform, env.maxTouchPoints);
}

/**
 * A phone or tablet (05 §8): userAgentData.mobile where Client Hints exist; else iOS/iPadOS, a mobile UA, or a
 * touch screen as the only pointer. Android tablets report mobile: false in Client Hints, so an Android UA counts
 * too. M1 doesn't share from these devices; they connect with role viewer.
 */
export function isHandheld(env: DeviceEnv = deviceEnv()): boolean {
  if (env.uaData?.mobile === true) return true;
  if (isIOSDevice(env) || isMobileUA(env.ua)) return true;
  if (env.uaData) return false;
  return env.matches('(pointer: coarse)') && !env.matches('(any-pointer: fine)');
}

/** Whether this page runs as an installed app (display-mode standalone, or iOS's navigator.standalone). */
export function isStandalone(env: DeviceEnv = deviceEnv()): boolean {
  return env.matches('(display-mode: standalone)') || globalThis.navigator.standalone === true;
}

/**
 * Element fullscreen where the Fullscreen API works; video-only fullscreen on iPhone (webkitEnterFullscreen);
 * else none, and the viewer uses a CSS pseudo-fullscreen (05 §8, §12.5).
 */
export function fullscreenSupport(): 'element' | 'video-only' | 'none' {
  const doc = globalThis.document as Document & { webkitFullscreenEnabled?: boolean };
  if (doc.fullscreenEnabled || doc.webkitFullscreenEnabled === true) return 'element';
  const video = (globalThis as { HTMLVideoElement?: typeof HTMLVideoElement }).HTMLVideoElement;
  if (typeof video?.prototype.webkitEnterFullscreen === 'function') return 'video-only';
  return 'none';
}

/** Picture-in-picture, never on iOS in M1 (plan: unreliable there). */
export function pipSupport(env: DeviceEnv = deviceEnv()): boolean {
  // lib.dom says boolean, but browsers without PiP (and jsdom) leave it undefined.
  return (globalThis.document.pictureInPictureEnabled as boolean | undefined) === true && !isIOSDevice(env);
}

/** The Screen Wake Lock API is there (it may still refuse, e.g. in Low Power Mode). */
export function wakeLockSupport(): boolean {
  return 'wakeLock' in globalThis.navigator;
}
